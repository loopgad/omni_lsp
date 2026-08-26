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
	HeaderContentType   = "Content-Type"

	// DefaultMaxMessageSize is the default inbound message cap per goal.md §C1
	// (64 MiB). It is configurable per-Codec via the MaxMessageSize field.
	DefaultMaxMessageSize = 64 * 1024 * 1024
)

// Codec handles encoding and decoding of JSON-RPC messages over a stream.
// This implements the LSP base protocol framing (Content-Length header based).
type Codec struct {
	// MaxMessageSize caps the accepted Content-Length for inbound messages.
	// Values <=0 select DefaultMaxMessageSize.
	MaxMessageSize int
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

// ReadMessage reads a single JSON-RPC message from the reader.
// It parses Content-Length header, then reads exactly that many bytes.
func (c *Codec) ReadMessage(r io.Reader) (*Message, error) {
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
		return nil, fmt.Errorf("jsonrpc: message too large: %d bytes (max %d)", contentLength, c.maxMessageSize())
	}

	body := make([]byte, contentLength)
	_, err = io.ReadFull(r, body)
	if err != nil {
		return nil, fmt.Errorf("jsonrpc: failed to read message body: %w", err)
	}

	var msg Message
	if err := json.Unmarshal(body, &msg); err != nil {
		// Return a parse error but with raw body for diagnostics.
		return nil, &CodecError{
			Code:    ParseError,
			Message: fmt.Sprintf("parse error: %v", err),
			Raw:     body,
		}
	}

	return &msg, nil
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

	// Parse headers.
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
			n, err := strconv.Atoi(val)
			if err != nil {
				return 0, fmt.Errorf("jsonrpc: invalid Content-Length value: %q", val)
			}
			contentLength = n
		}
	}

	if contentLength < 0 {
		return 0, fmt.Errorf("jsonrpc: missing Content-Length header")
	}

	return contentLength, nil
}

// CodecError is an error returned by the codec with optional raw data.
type CodecError struct {
	Code    int
	Message string
	Raw     []byte
}

func (e *CodecError) Error() string { return e.Message }
