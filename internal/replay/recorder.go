package replay

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
	"github.com/omnilsp/omni/internal/transport"
)

// RecorderTransport wraps a Transport and records every message in both
// directions to a JSONL session file (§P9). revFn samples the server's
// snapshot revision at capture time for the SnapRev column.
//
// A recording write failure never aborts or corrupts the live session: the
// message still flows, later writes are still attempted, and the first
// failure is retained for Err() so callers can detect a partial recording.
type RecorderTransport struct {
	transport.Transport

	mu       sync.Mutex
	f        *os.File
	w        *bufio.Writer
	seq      int
	firstErr error
	revFn    func() uint64
}

// NewRecorder opens path for writing, emits the meta header, and returns the
// wrapping transport. Close the recorder (not just the inner transport) when
// the session ends.
func NewRecorder(inner transport.Transport, path string, meta Meta, revFn func() uint64) (*RecorderTransport, error) {
	meta.FormatVersion = FormatVersion
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	r := &RecorderTransport{
		Transport: inner,
		f:         f,
		w:         bufio.NewWriter(f),
		revFn:     revFn,
	}
	if err := r.log("meta", json.RawMessage(mustJSON(meta))); err != nil {
		f.Close()
		return nil, err
	}
	return r, nil
}

func (r *RecorderTransport) Read(ctx context.Context) (*jsonrpc.Message, error) {
	msg, err := r.Transport.Read(ctx)
	if msg != nil {
		_ = r.log("in", raw(msg))
	}
	return msg, err
}

func (r *RecorderTransport) Write(ctx context.Context, msg *jsonrpc.Message) error {
	if msg != nil {
		_ = r.log("out", raw(msg))
	}
	return r.Transport.Write(ctx, msg)
}

// Close flushes and releases the session file.
func (r *RecorderTransport) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.w != nil {
		if err := r.w.Flush(); err != nil && r.firstErr == nil {
			r.firstErr = err
		}
		r.w = nil
	}
	return r.f.Close()
}

// Err returns the first recording write failure, if any. Later failures do
// not overwrite it; subsequent writes are still attempted.
func (r *RecorderTransport) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.firstErr
}

func (r *RecorderTransport) log(dir string, payload json.RawMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	var rev uint64
	if r.revFn != nil {
		rev = r.revFn()
	}
	err := json.NewEncoder(r.w).Encode(Entry{
		Seq: r.seq, Dir: dir, SnapRev: rev, Payload: payload,
	})
	if err != nil && r.firstErr == nil {
		r.firstErr = fmt.Errorf("replay: record %s #%d: %w", dir, r.seq, err)
	}
	return err
}

func raw(m *jsonrpc.Message) json.RawMessage {
	b, _ := json.Marshal(m)
	return b
}
