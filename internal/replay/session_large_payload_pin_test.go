package replay

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
)

// Pin for round20/bugB bug 4: the recorder and Save accept any payload the
// transport does — inbound messages are capped by the jsonrpc codec at
// DefaultMaxMessageSize (64 MiB) — but LoadSession read JSONL lines through a
// bufio.Scanner capped at 8 MiB, so one legal large didChange (a full-file
// content sync, well under the transport cap) was recorded into a session
// file that could never be loaded again: recording produced a dead archive,
// violating the "a recording is replayable" format invariant.
func TestLoadSessionAcceptsTransportSizedPayload(t *testing.T) {
	const payloadSize = 9 * 1024 * 1024 // > old 8 MiB scanner cap, < 64 MiB transport cap
	if payloadSize >= jsonrpc.DefaultMaxMessageSize {
		t.Fatalf("payload %d must stay under the transport cap %d", payloadSize, jsonrpc.DefaultMaxMessageSize)
	}
	payload := mustJSON(map[string]any{
		"jsonrpc": "2.0",
		"method":  "textDocument/didChange",
		"params": map[string]any{
			"textDocument":   map[string]any{"uri": "file:///ws/big.txt", "version": 7},
			"contentChanges": []any{map[string]any{"text": strings.Repeat("x", payloadSize)}},
		},
	})
	session := &Session{
		Meta:    Meta{FormatVersion: FormatVersion},
		Entries: []Entry{{Seq: 1, Dir: "in", Payload: payload}},
	}
	path := filepath.Join(t.TempDir(), "large.replay")
	if err := Save(path, session); err != nil {
		t.Fatalf("Save a legal transport-sized payload: %v", err)
	}

	loaded, err := LoadSession(path)
	if err != nil {
		t.Fatalf("LoadSession rejected a session the recorder legally produced: %v", err)
	}
	if len(loaded.Entries) != 1 {
		t.Fatalf("LoadSession returned %d entries, want 1", len(loaded.Entries))
	}
	if string(loaded.Entries[0].Payload) != string(payload) {
		t.Fatalf("payload did not round-trip byte for byte (got %d bytes, want %d)", len(loaded.Entries[0].Payload), len(payload))
	}
}
