package server

import (
	"encoding/json"

	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
)

// §C8 work-done progress: when the client supplies a workDoneToken, long
// queries report begin/end so the editor shows activity instead of silence.
// The server never invents tokens — client-supplied only, which keeps the
// protocol surface minimal (no window/workDoneProgress/create round-trip).

// notifyClient pushes a notification to the client over the active transport.
func (s *Server) notifyClient(method string, params any) {
	raw, err := json.Marshal(params)
	if err != nil {
		return
	}
	_ = s.send(jsonrpc.NewNotification(method, raw))
}

// extractWorkDoneToken pulls the optional workDoneToken from request params.
// It returns the token as raw JSON rather than a string because LSP 3.17
// defines ProgressToken as integer | string, and the token has to go back out
// exactly as it arrived. Decoding to string first would drop a numeric token
// on the floor, and the client would then never see the $/progress pair it
// asked for.
func extractWorkDoneToken(raw json.RawMessage) json.RawMessage {
	var p struct {
		WorkDoneToken json.RawMessage `json:"workDoneToken"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil
	}
	// An explicit null is as absent as a missing key: there is no token to
	// report progress against.
	if len(p.WorkDoneToken) == 0 || string(p.WorkDoneToken) == "null" {
		return nil
	}
	return p.WorkDoneToken
}

// progressBegin reports a $/progress begin for the given token.
func (s *Server) progressBegin(token json.RawMessage, title string) {
	if len(token) == 0 {
		return
	}
	s.notifyClient("$/progress", map[string]any{
		"token": token,
		"value": map[string]any{"kind": "begin", "title": title},
	})
}

// progressEnd reports a $/progress end for the given token.
func (s *Server) progressEnd(token json.RawMessage) {
	if len(token) == 0 {
		return
	}
	s.notifyClient("$/progress", map[string]any{"token": token, "value": map[string]any{"kind": "end"}})
}
