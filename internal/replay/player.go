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
	"sync/atomic"
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

	expected atomic.Int64 // fed request count: the response floor before EOF

	outMu   sync.Mutex // guards out: server goroutine writes, reader polls
	endOnce sync.Once
}

func newPlayerTransport() *playerTransport {
	return &playerTransport{
		// Buffered so callers may Feed the whole script before srv.Run starts.
		// ponytail: fixed 4k depth — a recorded session exceeding it should be
		// split; upgrade to an unbounded queue only when real sessions hit it.
		in:     make(chan *jsonrpc.Message, 4096),
		done:   make(chan struct{}),
		closed: make(chan struct{}),
	}
}

func (p *playerTransport) feed(msg *jsonrpc.Message) {
	if msg.ID != nil && msg.Method != "" {
		p.expected.Add(1) // requests must each produce exactly one response
	}
	select {
	case p.in <- msg:
	case <-p.closed:
	}
}

func (p *playerTransport) endFeed() { close(p.done) }

func (p *playerTransport) Read(ctx context.Context) (*jsonrpc.Message, error) {
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
			// Feed ended: wait until every fed request has been answered
			// (async workers may lag the synchronous inline path), then end.
			p.drainRequests(ctx)
			p.endOnce.Do(func() { close(p.closed) })
			// Stream ended cleanly; server.Run maps io.EOF to a graceful stop.
			return nil, io.EOF
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// drainRequests blocks until outLen covers every request we fed. Server-
// initiated notifications make this an over-approximation of completion —
// acceptable: they only ever make us wait marginally longer, never shorter.
// ponytail: count-based; switch to per-ID response tracking if replays show skew.
func (p *playerTransport) drainRequests(ctx context.Context) {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if int64(p.outLen()) >= p.expected.Load() {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (p *playerTransport) Write(_ context.Context, msg *jsonrpc.Message) error {
	p.outMu.Lock()
	p.out = append(p.out, msg)
	p.outMu.Unlock()
	return nil
}

func (p *playerTransport) outLen() int {
	p.outMu.Lock()
	defer p.outMu.Unlock()
	return len(p.out)
}

func (p *playerTransport) Close() error { close(p.closed); return nil }

// Done satisfies transport.Transport: closed when the player is torn down.
func (p *playerTransport) Done() <-chan struct{} { return p.closed }

// Runner drives a replay: it feeds every recorded "in" entry to srv in
// logical order, collects the responses, and returns the produced "out"
// messages for comparison with the recording.
//
// The runner must be wired through a real server.Run call by the caller:
//
//	pt := replay.NewPlayer()
//	go func() { defer pt.endFeed(); for _, e := range ins { pt.feed(...) } }()
//	srv.Run(ctx, pt)
type Player struct {
	t *playerTransport
}

func NewPlayer() *Player {
	return &Player{t: newPlayerTransport()}
}

// Transport exposes the player's transport for srv.Run.
func (p *Player) Transport() *playerTransport { return p.t }

// Feed queues one message; EndFeed signals EOF after all inputs are queued.
func (p *Player) Feed(msg *jsonrpc.Message) { p.t.feed(msg) }
func (p *Player) EndFeed()                  { p.t.endFeed() }

// Out returns the responses collected from the server so far.
func (p *Player) Out() []*jsonrpc.Message {
	p.t.outMu.Lock()
	defer p.t.outMu.Unlock()
	return append([]*jsonrpc.Message(nil), p.t.out...)
}

// awaitQuiescent blocks until the server's output stream has been stable for
// 200ms (or the deadline passes) — async worker responses may still be in
// flight when the input script drains.
func (p *Player) awaitQuiescent(max time.Duration) {
	deadline := time.Now().Add(max)
	last, stable := -1, time.Duration(0)
	for time.Now().Before(deadline) {
		n := p.t.outLen()
		if n == last {
			stable += 20 * time.Millisecond
		} else {
			stable = 0
			last = n
		}
		if stable >= 200*time.Millisecond {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// CompareOut verifies that the replayed output matches the recording
// message-for-message after normalization (volatile fields stripped).
// Returns nil when identical, otherwise a human-readable diff summary.
func (p *Player) CompareOut(recorded []Entry) error {
	p.awaitQuiescent(5 * time.Second)
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
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if volatileKeys[k] {
				continue
			}
			out[k] = stripVolatile(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = stripVolatile(val)
		}
		return out
	default:
		return v
	}
}
