// Package replay records and replays JSON-RPC sessions deterministically.
//
// Concurrency model: single-writer per connection; reads served from captured
// snapshots; no goroutine escapes the owning supervisor.
//
// Invariants:
//  1. Fail-closed semantics: missing inputs produce refusals, never guesses.
//  2. Wire messages are versioned surfaces; changes require schema bumps.
//  3. Errors carry typed identity per internal/errors conventions.
package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"io"

	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
)

// playerTransport feeds recorded "in" entries to a server and collects the
// "out" responses it writes. Read blocks until a message is queued, the feed
// completes (EOF), or the context dies — mirroring real transport semantics.
type playerTransport struct {
	in     chan *jsonrpc.Message
	out    []*jsonrpc.Message
	done   chan struct{}
	closed chan struct{}

	expected  map[string]int // request count by wire ID; notifications do not count
	responses map[string]int // matching responses observed by Write
	changed   chan struct{}  // output/request progress notification
	feedOnce  sync.Once
	drainWait time.Duration
	outWait   time.Duration

	outMu           sync.Mutex // guards output messages and binding state
	outBindings     map[string]SemanticResponseBinding
	identities      map[string]SemanticIdentity
	requestIdentity map[string]SemanticIdentity
	outBinding      []*SemanticResponseBinding // aligned with out
	endOnce         sync.Once
}

func newPlayerTransport() *playerTransport {
	return &playerTransport{
		// Bounded buffering allows normal read-ahead while keeping replay
		// backpressure when a server falls behind.
		in:              make(chan *jsonrpc.Message, 4096),
		done:            make(chan struct{}),
		closed:          make(chan struct{}),
		expected:        make(map[string]int),
		responses:       make(map[string]int),
		changed:         make(chan struct{}, 1),
		drainWait:       10 * time.Second,
		outWait:         10 * time.Second,
		outBindings:     make(map[string]SemanticResponseBinding),
		identities:      make(map[string]SemanticIdentity),
		requestIdentity: make(map[string]SemanticIdentity),
	}
}

func (p *playerTransport) feed(ctx context.Context, msg *jsonrpc.Message) error {
	select {
	case <-p.closed:
		return io.ErrClosedPipe
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	requestKey := ""
	if msg != nil && msg.IsRequest() {
		requestKey = requestIDKey(*msg.ID)
		p.outMu.Lock()
		p.expected[requestKey]++
		p.outMu.Unlock()
	}
	select {
	case p.in <- msg:
		return nil
	case <-p.closed:
		if requestKey != "" {
			p.outMu.Lock()
			p.expected[requestKey]--
			p.outMu.Unlock()
		}
		return io.ErrClosedPipe
	case <-ctx.Done():
		if requestKey != "" {
			p.outMu.Lock()
			p.expected[requestKey]--
			p.outMu.Unlock()
		}
		return ctx.Err()
	}
}

func (p *playerTransport) endFeed() { p.feedOnce.Do(func() { close(p.done) }) }

func (p *playerTransport) Read(ctx context.Context) (*jsonrpc.Message, error) {
	select {
	case <-p.closed:
		return nil, io.EOF
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	// Drained-input fast path must come FIRST: with a closed done channel,
	// Go's select picks among ready cases at random, so the done branch can
	// fire while buffered inputs remain.
	for {
		select {
		case msg := <-p.in:
			return msg, nil
		default:
		}
		select {
		case msg := <-p.in:
			return msg, nil
		case <-p.done:
			// Feed ended: wait until every fed request has a matching response
			// (async workers may lag the synchronous inline path), then end.
			drainErr := p.drainRequests(ctx)
			p.endOnce.Do(func() { close(p.closed) })
			if drainErr != nil {
				return nil, drainErr
			}
			// Stream ended cleanly; server.Run maps io.EOF to a graceful stop.
			return nil, io.EOF
		case <-p.closed:
			// An explicit Close must also unblock a Read waiting for input.
			return nil, io.EOF
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// drainRequests blocks until every fed request has a response with the same
// wire ID. Notifications and unrelated server outputs cannot satisfy it.
func (p *playerTransport) drainRequests(ctx context.Context) error {
	timer := time.NewTimer(p.drainWait)
	defer timer.Stop()
	for {
		p.outMu.Lock()
		pending, total := p.pendingRequestsLocked()
		p.outMu.Unlock()
		if pending == 0 {
			return nil
		}
		select {
		case <-p.changed:
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return fmt.Errorf("replay: incomplete request drain: received responses for %d of %d requests after %s", total-pending, total, p.drainWait)
		}
	}
}

func (p *playerTransport) pendingRequestsLocked() (pending, total int) {
	for key, expected := range p.expected {
		total += expected
		answered := p.responses[key]
		if answered < expected {
			pending += expected - answered
		}
	}
	return pending, total
}

func (p *playerTransport) signalChange() {
	select {
	case p.changed <- struct{}{}:
	default:
	}
}

func (p *playerTransport) Write(_ context.Context, msg *jsonrpc.Message) error {
	p.outMu.Lock()
	var binding *SemanticResponseBinding
	if msg != nil && msg.IsResponse() {
		key := requestIDKey(*msg.ID)
		if p.responses[key] < p.expected[key] {
			p.responses[key]++
		}
		if recorded, ok := p.outBindings[key]; ok {
			copy := recorded
			binding = &copy
			delete(p.outBindings, key)
		}
	}
	p.out = append(p.out, msg)
	p.outBinding = append(p.outBinding, binding)
	p.outMu.Unlock()
	p.signalChange()
	return nil
}

func (p *playerTransport) Close() error {
	p.endOnce.Do(func() { close(p.closed) })
	return nil
}

// Done satisfies transport.Transport: closed when the player is torn down.
func (p *playerTransport) Done() <-chan struct{} { return p.closed }

// Replay feeds recorded inputs in order and waits for each recorded output
// before releasing later inputs. Start the server.Run call concurrently first.
// EndFeed is called after the complete recording has been applied.
func (p *Player) Replay(ctx context.Context, entries []Entry) error {
	if p == nil || p.t == nil {
		return fmt.Errorf("replay: nil player")
	}
	outputIndex := 0
	for _, entry := range entries {
		switch entry.Dir {
		case "in":
			var msg jsonrpc.Message
			if err := json.Unmarshal(entry.Payload, &msg); err != nil {
				return fmt.Errorf("replay: entry %d: %w", entry.Seq, err)
			}
			if err := p.t.feed(ctx, &msg); err != nil {
				return fmt.Errorf("replay: feed entry %d: %w", entry.Seq, err)
			}
		case "out":
			var want jsonrpc.Message
			if err := json.Unmarshal(entry.Payload, &want); err != nil {
				return fmt.Errorf("replay: recorded out entry %d unparsable: %w", entry.Seq, err)
			}
			if err := p.t.waitForOutput(ctx, outputIndex, entry.Seq, &want); err != nil {
				return err
			}
			outputIndex++
		}
	}
	p.EndFeed()
	return nil
}

type Player struct {
	t *playerTransport
}

func NewPlayer() *Player {
	return &Player{t: newPlayerTransport()}
}

// Transport exposes the player's transport for srv.Run.
func (p *Player) Transport() *playerTransport { return p.t }

// Feed queues one message; EndFeed signals EOF after all inputs are queued.
func (p *Player) Feed(msg *jsonrpc.Message) { _ = p.t.feed(context.Background(), msg) }

// FeedContext queues one message unless the context ends or the transport is
// closed. It is useful to feed large recordings without losing backpressure.
func (p *Player) FeedContext(ctx context.Context, msg *jsonrpc.Message) error {
	if p == nil || p.t == nil {
		return fmt.Errorf("replay: nil player")
	}
	return p.t.feed(ctx, msg)
}

func (p *Player) EndFeed() { p.t.endFeed() }

func (p *playerTransport) waitForOutput(ctx context.Context, index, seq int, want *jsonrpc.Message) error {
	timer := time.NewTimer(p.outWait)
	defer timer.Stop()
	for {
		p.outMu.Lock()
		if len(p.out) > index {
			got := p.out[index]
			p.outMu.Unlock()
			if got == nil {
				return fmt.Errorf("replay: response %d diverges: replayed output is nil", index)
			}
			actualJSON, _ := json.Marshal(normalize(got))
			wantJSON, _ := json.Marshal(normalize(want))
			if string(actualJSON) != string(wantJSON) {
				return fmt.Errorf("replay: response %d diverges:\n  replayed: %s\n  recorded: %s", index, actualJSON, wantJSON)
			}
			return nil
		}
		p.outMu.Unlock()
		select {
		case <-p.changed:
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return fmt.Errorf("replay: timed out waiting for output recorded at entry %d after %s", seq, p.outWait)
		}
	}
}

// RegisterSemanticIdentity makes the replay server's independently observed
// identity available to subsequent response bindings.
func (p *Player) RegisterSemanticIdentity(id jsonrpc.RequestID, identity SemanticIdentity) error {
	if p == nil || p.t == nil {
		return fmt.Errorf("replay: nil player")
	}
	return p.t.RegisterSemanticIdentity(id, identity)
}

// RegisterSemanticIdentity is exposed on the transport so the server can
// record request-specific identity through its active transport interface.
func (p *playerTransport) RegisterSemanticIdentity(id jsonrpc.RequestID, identity SemanticIdentity) error {
	p.outMu.Lock()
	defer p.outMu.Unlock()
	requestKey := requestIDKey(id)
	if _, exists := p.requestIdentity[requestKey]; exists {
		return fmt.Errorf("replay: semantic identity for request %s already exists", requestKey)
	}
	updated := make(map[string]SemanticIdentity, len(p.identities)+1)
	for key, known := range p.identities {
		updated[key] = known
	}
	_, err := registerSemanticIdentity(updated, identity)
	if err != nil {
		return err
	}
	p.identities = updated
	p.requestIdentity[requestKey] = cloneSemanticIdentity(identity)
	return nil
}

// BindSemanticResponse registers the provenance that the server observed for
// a request. Call it after the request's semantic result is known and before
// the corresponding response is written to the player transport.
func (p *Player) BindSemanticResponse(id jsonrpc.RequestID, semantic bool, generation uint64, indexContentDigest string) error {
	if p == nil || p.t == nil {
		return fmt.Errorf("replay: nil player")
	}
	return p.t.BindSemanticResponse(id, semantic, generation, indexContentDigest)
}

// BindSemanticResponse is exposed on the transport for the server's response
// write path. It binds the upcoming Write by its JSON-RPC response ID.
func (p *playerTransport) BindSemanticResponse(id jsonrpc.RequestID, semantic bool, generation uint64, indexContentDigest string) error {
	p.outMu.Lock()
	defer p.outMu.Unlock()
	key := requestIDKey(id)
	identity, exists := p.requestIdentity[key]
	if !exists {
		return fmt.Errorf("%w: register the response identity before binding request %s", ErrSemanticIdentityUnverified, requestIDKey(id))
	}
	binding, err := newSemanticResponseBinding(id, semantic, generation, indexContentDigest, &identity)
	if err != nil {
		return err
	}
	if _, exists := p.outBindings[key]; exists {
		return fmt.Errorf("replay: response binding for request %s already exists", key)
	}
	p.outBindings[key] = binding
	delete(p.requestIdentity, key)
	return nil
}

// Out returns the responses collected from the server so far.
func (p *Player) Out() []*jsonrpc.Message {
	p.t.outMu.Lock()
	defer p.t.outMu.Unlock()
	return append([]*jsonrpc.Message(nil), p.t.out...)
}

func (p *Player) outputBindings() []*SemanticResponseBinding {
	if p == nil || p.t == nil {
		return nil
	}
	p.t.outMu.Lock()
	defer p.t.outMu.Unlock()
	return append([]*SemanticResponseBinding(nil), p.t.outBinding...)
}

// CompareOut verifies that the replayed output matches the recording
// message-for-message after normalization (volatile fields stripped).
// Returns nil when identical, otherwise a human-readable diff summary.
func (p *Player) CompareOut(recorded []Entry) error {
	got := p.Out()
	var want []*jsonrpc.Message
	for _, e := range recorded {
		if e.Dir != "out" {
			continue
		}
		var m jsonrpc.Message
		if err := json.Unmarshal(e.Payload, &m); err != nil {
			return fmt.Errorf("replay: recorded out entry %d unparsable: %w", e.Seq, err)
		}
		want = append(want, &m)
	}
	if len(got) != len(want) {
		return fmt.Errorf("replay: response count mismatch: got %d, recorded %d", len(got), len(want))
	}
	for i := range got {
		a, _ := json.Marshal(normalize(got[i]))
		b, _ := json.Marshal(normalize(want[i]))
		if string(a) != string(b) {
			return fmt.Errorf("replay: response %d diverges:\n  replayed: %s\n  recorded: %s", i, a, b)
		}
	}
	return nil
}

// CompareSession verifies the pinned semantic identity, checks recorded and
// replayed response provenance, and compares the response stream. Legacy
// recordings remain usable through LoadSession and CompareOut, but cannot
// establish complete semantic reproduction without response bindings.
func (p *Player) CompareSession(session *Session, available *SemanticIdentity) (ReproductionStatus, error) {
	if p == nil {
		return ReproductionPartialUnverified, fmt.Errorf("replay: nil player")
	}
	if err := session.VerifyResponseBindings(available); err != nil {
		return ReproductionPartialUnverified, err
	}
	status := ReproductionIdentityVerified
	if !session.usesSemanticGeneration() && !session.hasSemanticResponses() &&
		completeSemanticIdentity(session.Meta.SemanticIdentity) && available != nil {
		var err error
		status, err = session.VerifySemanticReproduction(available)
		if err != nil {
			return status, err
		}
	}
	if err := p.CompareOut(session.Entries); err != nil {
		return status, err
	}
	actualBindings := p.outputBindings()
	pendingMethods := make(map[string]string)
	outputIndex := 0
	for _, entry := range session.Entries {
		if entry.Dir == "in" {
			var request jsonrpc.Message
			if err := json.Unmarshal(entry.Payload, &request); err != nil {
				return status, fmt.Errorf("replay: recorded in entry %d unparsable: %w", entry.Seq, err)
			}
			if request.IsRequest() {
				pendingMethods[requestIDKey(*request.ID)] = request.Method
			}
			continue
		}
		if entry.Dir != "out" {
			continue
		}
		var msg jsonrpc.Message
		if err := json.Unmarshal(entry.Payload, &msg); err != nil {
			return status, fmt.Errorf("replay: recorded out entry %d unparsable: %w", entry.Seq, err)
		}
		if msg.IsResponse() {
			key := requestIDKey(*msg.ID)
			method := pendingMethods[key]
			delete(pendingMethods, key)
			needsBinding := responseNeedsSemanticBinding(method) || entry.SemanticBinding != nil
			if needsBinding {
				if outputIndex >= len(actualBindings) || actualBindings[outputIndex] == nil {
					return status, fmt.Errorf("%w: replayed response %d has no binding", ErrSemanticResponseBindingUnverified, entry.Seq)
				}
				actual := actualBindings[outputIndex]
				if err := verifySemanticResponseBinding(actual, available); err != nil {
					return status, fmt.Errorf("replayed response %d: %w", entry.Seq, err)
				}
				if !sameResponseBinding(entry.SemanticBinding, actual) {
					return status, fmt.Errorf("%w: replayed response %d provenance differs", ErrSemanticResponseBindingMismatch, entry.Seq)
				}
			}
		}
		outputIndex++
	}
	return ReproductionComplete, nil
}

func sameResponseBinding(a, b *SemanticResponseBinding) bool {
	return a != nil && b != nil && a.RequestID.Equals(b.RequestID) && a.Kind == b.Kind &&
		a.Generation == b.Generation && a.IndexContentDigest == b.IndexContentDigest &&
		a.ToolIdentityDigest == b.ToolIdentityDigest
}

// normalize deep-copies the message and strips volatile fields ("at" style
// timestamps inside results) that legitimately differ between runs.
func normalize(m *jsonrpc.Message) *jsonrpc.Message {
	cp := *m
	if cp.Result != nil {
		var v any
		if err := json.Unmarshal(cp.Result, &v); err == nil {
			stripped := stripVolatile(v)
			if b, err := json.Marshal(stripped); err == nil {
				cp.Result = b
			}
		}
	}
	return &cp
}

var volatileKeys = map[string]bool{"at": true, "At": true, "ts": true, "timestamp": true}

func stripVolatile(v any) any {
	return stripVolatileEvidence(v, false)
}

func stripVolatileEvidence(v any, evidence bool) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if volatileKeys[k] {
				continue
			}
			// Evidence uses the exported Go field spelling. Normalize only
			// that typed telemetry shape, not arbitrary user result fields.
			if evidence && k == "Timestamp" && t["Kind"] != nil && t["Snapshot"] != nil && t["Assurance"] != nil {
				if text, ok := val.(string); ok {
					if _, err := time.Parse(time.RFC3339Nano, text); err == nil {
						continue
					}
				}
			}
			isEvidence := k == "evidence" && t["method"] != nil && t["status"] != nil && t["completeness"] != nil
			out[k] = stripVolatileEvidence(val, isEvidence)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = stripVolatileEvidence(val, evidence)
		}
		return out
	default:
		return v
	}
}
