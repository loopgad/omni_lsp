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
func extractWorkDoneToken(raw json.RawMessage) string {
	var p struct {
		WorkDoneToken json.Token `json:"workDoneToken"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return ""
	}
	if s, ok := p.WorkDoneToken.(string); ok {
		return s
	}
	return ""
}

// progressBegin reports a $/progress begin for the given token.
func (s *Server) progressBegin(token, title string) {
	if token == "" {
		return
	}
	s.notifyClient("$/progress", map[string]any{
		"token": token,
		"value": map[string]any{"kind": "begin", "title": title},
	})
}

// progressEnd reports a $/progress end for the given token.
func (s *Server) progressEnd(token string) {
	if token == "" {
		return
	}
	s.notifyClient("$/progress", map[string]any{"token": token, "value": map[string]any{"kind": "end"}})
}
