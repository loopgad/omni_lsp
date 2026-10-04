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

	mu              sync.Mutex
	f               *os.File
	w               *bufio.Writer
	seq             int
	firstErr        error
	revFn           func() uint64
	bindings        map[string]SemanticResponseBinding
	identities      map[string]SemanticIdentity
	requestIdentity map[string]SemanticIdentity
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
		Transport:       inner,
		f:               f,
		w:               bufio.NewWriter(f),
		revFn:           revFn,
		bindings:        make(map[string]SemanticResponseBinding),
		identities:      make(map[string]SemanticIdentity),
		requestIdentity: make(map[string]SemanticIdentity),
	}
	if validRegisteredSemanticIdentity(meta.SemanticIdentity) {
		_, _ = registerSemanticIdentity(r.identities, *meta.SemanticIdentity)
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
		_ = r.logOutbound(msg)
	}
	return r.Transport.Write(ctx, msg)
}

// RegisterSemanticIdentity associates an identity with the request whose
// response will use it. Generation
// identities are recorded once per generation; tool-only identities are
// recorded once per distinct tool set. Call it before binding a response that
// used the identity. The event lets recordings started before indexing pin
// the identity that later responses actually consumed.
func (r *RecorderTransport) RegisterSemanticIdentity(id jsonrpc.RequestID, identity SemanticIdentity) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	requestKey := requestIDKey(id)
	if _, exists := r.requestIdentity[requestKey]; exists {
		return fmt.Errorf("replay: semantic identity for request %s already exists", requestKey)
	}
	updated := make(map[string]SemanticIdentity, len(r.identities)+1)
	for key, known := range r.identities {
		updated[key] = known
	}
	key, err := registerSemanticIdentity(updated, identity)
	if err != nil {
		return err
	}
	if _, exists := r.identities[key]; !exists {
		if err := r.logLocked("identity", mustJSON(identity), nil); err != nil {
			return err
		}
		r.identities = updated
	}
	r.requestIdentity[requestKey] = cloneSemanticIdentity(identity)
	return nil
}

// BindSemanticResponse records the provenance selected while handling the
// request. Call RegisterSemanticIdentity first, then call this after the
// request's semantic result is known and before Write sends the response.
// semantic=false explicitly marks a live response that used no index facts;
// semantic=true requires the exact immutable generation and content digest.
func (r *RecorderTransport) BindSemanticResponse(id jsonrpc.RequestID, semantic bool, generation uint64, indexContentDigest string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := requestIDKey(id)
	identity, exists := r.requestIdentity[key]
	if !exists {
		return fmt.Errorf("%w: register the response identity before binding request %s", ErrSemanticIdentityUnverified, requestIDKey(id))
	}
	binding, err := newSemanticResponseBinding(id, semantic, generation, indexContentDigest, &identity)
	if err != nil {
		return err
	}
	if _, exists := r.bindings[key]; exists {
		return fmt.Errorf("replay: response binding for request %s already exists", key)
	}
	r.bindings[key] = binding
	delete(r.requestIdentity, key)
	return nil
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
	return r.logLocked(dir, payload, nil)
}

func (r *RecorderTransport) logOutbound(msg *jsonrpc.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	var binding *SemanticResponseBinding
	if msg.IsResponse() {
		key := requestIDKey(*msg.ID)
		if recorded, ok := r.bindings[key]; ok {
			copy := recorded
			binding = &copy
			delete(r.bindings, key)
		}
	}
	return r.logLocked("out", raw(msg), binding)
}

func (r *RecorderTransport) logLocked(dir string, payload json.RawMessage, binding *SemanticResponseBinding) error {
	r.seq++
	var rev uint64
	if r.revFn != nil {
		rev = r.revFn()
	}
	err := json.NewEncoder(r.w).Encode(Entry{
		Seq: r.seq, Dir: dir, SnapRev: rev, SemanticBinding: binding, Payload: payload,
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
