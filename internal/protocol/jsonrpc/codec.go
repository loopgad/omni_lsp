package jsonrpc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const (
	HeaderContentLength = "Content-Length"

	// DefaultMaxMessageSize is the default inbound message cap per goal.md §C1
	// (64 MiB). It is configurable per-Codec via the MaxMessageSize field.
	DefaultMaxMessageSize = 64 * 1024 * 1024

	// DefaultMaxBatchElements is the default per-frame batch element cap per
	// goal.md §C1 (no unbounded allocation from attacker-controlled shape).
	// It is configurable per-Codec via the MaxBatchElements field. LSP 3.17
	// deprecates batches, so the cap is generous for the legitimate shapes
	// that remain. The cap also bounds the per-frame error-reply budget: an
	// invalid element owes at most one reply, so replies per frame can never
	// exceed the element cap.
	DefaultMaxBatchElements = 1024
)

// Codec handles encoding and decoding of JSON-RPC messages over a stream.
// This implements the LSP base protocol framing (Content-Length header based).
type Codec struct {
	// MaxMessageSize caps the accepted Content-Length for inbound messages.
	// Values <=0 select DefaultMaxMessageSize.
	MaxMessageSize int

	// MaxBatchElements caps the element count of one batch frame. A batch
	// over the cap is rejected whole as a ParseError (the per-stream fatal
	// class) instead of being amplified per element. Values <=0 select
	// DefaultMaxBatchElements.
	MaxBatchElements int

	// SkipOversizedFrames changes what an oversized inbound frame means.
	// Default false: a declared Content-Length above MaxMessageSize is a
	// per-stream fatal error (the §C1 posture — zero behavior change, X10).
	// When true, the frame body is drained and discarded exactly per the
	// declared length and ReadMessage/ReadFrame return a *FrameSkippedError:
	// the stream stays usable, so the caller may count the refusal and keep
	// reading. Bounded memory either way — the oversized body is never
	// allocated, only drained. Set this only where the size cap expresses a
	// memory quota (a tightened per-owner cap), not where it expresses a
	// protocol validity judgment.
	SkipOversizedFrames bool
}

// NewCodec creates a new codec with the default message size limit.
func NewCodec() *Codec {
	return &Codec{MaxMessageSize: DefaultMaxMessageSize}
}

func (c *Codec) maxMessageSize() int {
	if c.MaxMessageSize > 0 {
		return c.MaxMessageSize
	}
	return DefaultMaxMessageSize
}

func (c *Codec) maxBatchElements() int {
	if c.MaxBatchElements > 0 {
		return c.MaxBatchElements
	}
	return DefaultMaxBatchElements
}

// ReadMessage reads a single JSON-RPC message from the reader.
// It parses Content-Length header, then reads exactly that many bytes.
//
// ReadMessage is the single-message decoder: a batch (array) body cannot be
// represented as one *Message and fails with a typed ParseError. Frames are
// classified — batches included, per the JSON-RPC 2.0 §6 ruling — by
// ReadFrame; the transports read through ReadFrame, so an array body no
// longer reaches this decoder on a live session.
func (c *Codec) ReadMessage(r io.Reader) (*Message, error) {
	body, err := c.readBody(r)
	if err != nil {
		return nil, err
	}

	var msg Message
	if err := json.Unmarshal(body, &msg); err != nil {
		return nil, &CodecError{
			Code:    ParseError,
			Message: fmt.Sprintf("parse error: %v", err),
		}
	}

	return &msg, nil
}

// readBody reads one framed body: Content-Length header, size-cap checks,
// then exactly that many bytes. Shared by ReadMessage and ReadFrame so both
// enforce the same framing contract (§C1 cap included).
func (c *Codec) readBody(r io.Reader) ([]byte, error) {
	contentLength, err := c.readHeader(r)
	if err != nil {
		return nil, err
	}

	if contentLength <= 0 {
		return nil, fmt.Errorf("jsonrpc: invalid Content-Length: %d", contentLength)
	}
	// C1: reject oversized declared length BEFORE allocating the buffer so an
	// attacker-controlled header cannot force a large allocation.
	if contentLength > c.maxMessageSize() {
		if c.SkipOversizedFrames {
			// 审计批 2 发现 1：拒绝的是这一帧的内存分配，不是这条流。按声明
			// 长度排空 body（分段搬运，不按帧大小分配）后报告跳帧，调用方
			// 计数后可继续读。排空失败（流中断/EOF）仍是 fatal——那不是
			// 策略拒绝，是流本身断了。
			if derr := drainFrame(r, int64(contentLength)); derr != nil {
				return nil, fmt.Errorf("jsonrpc: failed to drain oversized frame body (%d bytes): %w", contentLength, derr)
			}
			return nil, &FrameSkippedError{Declared: contentLength, Cap: c.maxMessageSize()}
		}
		return nil, fmt.Errorf("jsonrpc: message too large: %d bytes (max %d)", contentLength, c.maxMessageSize())
	}

	body := make([]byte, contentLength)
	_, err = io.ReadFull(r, body)
	if err != nil {
		return nil, fmt.Errorf("jsonrpc: failed to read message body: %w", err)
	}

	return body, nil
}

// WriteMessage writes a JSON-RPC message to the writer with LSP framing.
func (c *Codec) WriteMessage(w io.Writer, msg *Message) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("jsonrpc: failed to marshal message: %w", err)
	}

	header := fmt.Sprintf("%s: %d\r\n\r\n", HeaderContentLength, len(body))
	if _, err := io.WriteString(w, header); err != nil {
		return fmt.Errorf("jsonrpc: failed to write header: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("jsonrpc: failed to write body: %w", err)
	}

	return nil
}

// readHeader parses headers from the stream, returning Content-Length.
func (c *Codec) readHeader(r io.Reader) (int, error) {
	var contentLength int = -1
	buf := make([]byte, 0, 256)
	tmp := make([]byte, 1)

	for {
		n, err := r.Read(tmp)
		if err != nil {
			return 0, fmt.Errorf("jsonrpc: failed to read header: %w", err)
		}
		if n == 0 {
			continue
		}

		buf = append(buf, tmp[0])

		// Check if we've reached end of headers (double CRLF).
		if len(buf) >= 4 && bytes.HasSuffix(buf, []byte("\r\n\r\n")) {
			break
		}
		// Also accept LF-only for robustness.
		if len(buf) >= 2 && bytes.HasSuffix(buf, []byte("\n\n")) {
			break
		}

		if len(buf) > 1024 {
			return 0, fmt.Errorf("jsonrpc: header too large")
		}
	}

	// Parse headers. A second Content-Length is frame-structure corruption,
	// not "last one wins": the old code overwrote contentLength with each
	// occurrence, so a hostile/desynchronized frame was consumed under an
	// arbitrary declared length and the leftover bytes were re-scanned as the
	// next header block — the stream desynced instead of failing cleanly. This
	// matches the plugin executor's readFrame, which rejects a repeated
	// Content-Length outright (executor.go「重复的 Content-Length 头」).
	// Additional headers remain tolerated (Content-Type etc.), and the LF-only
	// header terminator stays tolerated as well: both lanes (codec and plugin
	// reader) accept it today, a frame accepted under it still consumes exactly
	// Content-Length bytes, and TestCodecAcceptsToleratedFraming pins it as a
	// deliberate bend — narrowing it would cost sessions over a byte with no
	// desync to prevent.
	seenLength := false
	lines := strings.Split(strings.ReplaceAll(string(buf), "\r\n", "\n"), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		if strings.EqualFold(key, HeaderContentLength) {
			if seenLength {
				return 0, fmt.Errorf("jsonrpc: duplicate Content-Length header")
			}
			n, err := strconv.Atoi(val)
			if err != nil {
				return 0, fmt.Errorf("jsonrpc: invalid Content-Length value: %q", val)
			}
			contentLength = n
			seenLength = true
		}
	}

	if contentLength < 0 {
		return 0, fmt.Errorf("jsonrpc: missing Content-Length header")
	}

	return contentLength, nil
}

// CodecError is an error returned by the codec.
type CodecError struct {
	Code    int
	Message string
}

func (e *CodecError) Error() string { return e.Message }

// FrameSkippedError reports one inbound frame refused by MaxMessageSize whose
// body was drained and discarded (Codec.SkipOversizedFrames). The stream is
// still framed correctly at the byte after the skipped body — the caller may
// count the refusal (audit batch 2, finding 1: a quota refusal must be visible
// without killing the read loop) and read the next frame. It is not a
// CodecError: it is a per-frame policy outcome, not a protocol fault.
type FrameSkippedError struct {
	// Declared is the refused frame's Content-Length.
	Declared int
	// Cap is the codec's active MaxMessageSize at the refusal.
	Cap int
}

func (e *FrameSkippedError) Error() string {
	return "jsonrpc: oversized frame skipped: " +
		strconv.Itoa(e.Declared) + " bytes declared (max " + strconv.Itoa(e.Cap) + ")"
}

// drainFrame consumes exactly n bytes from r without buffering the frame
// (segmented hand-off to io.Discard; the pooled 8 KiB block is the only
// staging). io.CopyN reports io.EOF for a short frame; normalize it to
// ErrUnexpectedEOF — a truncated body means the stream itself broke, which is
// the caller's fatal class, not a skip.
func drainFrame(r io.Reader, n int64) error {
	if _, err := io.CopyN(io.Discard, r, n); err != nil {
		if err == io.EOF {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	return nil
}
