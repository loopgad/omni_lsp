package transport

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/omnilsp/omni/internal/protocol/jsonrpc"
)

// TestCodecStringNumericRequestIDKeepsTypeOnEcho pins bug round 20 A#14
// (圈 5): JSON-RPC 2.0 requires the response id to be the same value AND
// type as the request id. encoding/json accepts quoted numeric strings for a
// json.Number target, so the number-first decode erased IsStr for ids like
// "7" and the response went back on the wire as id:7 (number) — a full type
// change that strict id-matching clients (vscode-jsonrpc, LSP4J, Emacs
// lsp-mode, which really uses string-numeric ids) can never pair, leaving
// the request without a terminal response until client timeout. Pinned at
// the codec surface the stdio transport drives: decode the inbound frame,
// require the string id to survive decoding, echo a response through the
// codec, and require the wire to keep the quoted form.
func TestCodecStringNumericRequestIDKeepsTypeOnEcho(t *testing.T) {
	codec := jsonrpc.NewCodec()
	body := `{"jsonrpc":"2.0","id":"7","method":"shutdown","params":null}`
	frame := fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)

	fr, err := codec.ReadFrame(strings.NewReader(frame))
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if fr.Msg == nil || fr.Msg.ID == nil {
		t.Fatalf("decoded frame is not a single request: %+v", fr)
	}
	if !fr.Msg.ID.IsStr || fr.Msg.ID.Str != "7" {
		t.Fatalf("string-numeric request id erased during decode: IsStr=%v Num=%d Str=%q — response echo would change the wire id type", fr.Msg.ID.IsStr, fr.Msg.ID.Num, fr.Msg.ID.Str)
	}

	resp := jsonrpc.NewResponse(*fr.Msg.ID, nil)
	var out bytes.Buffer
	if err := codec.WriteMessage(&out, resp); err != nil {
		t.Fatalf("write response: %v", err)
	}
	wire := out.String()
	if !strings.Contains(wire, `"id":"7"`) || strings.Contains(wire, `"id":7`) {
		t.Fatalf("response wire changed the id type: %s", wire)
	}
}

// TestCodecRequestIDOtherShapesStayStable guards the id decode contract
// around the string-numeric fix: numeric ids stay numeric, plain string ids
// stay strings, and non-integer quoted ids keep their string type. (A null
// id inside a message is Message-level semantics — the custom
// Message.UnmarshalJSON leaves the ID pointer nil — out of this pin's
// scope.)
func TestCodecRequestIDOtherShapesStayStable(t *testing.T) {
	codec := jsonrpc.NewCodec()
	cases := []struct {
		idJSON    string
		checkName string
		isStr     bool
		isNull    bool
		num       int64
		str       string
	}{
		{`7`, "numeric id stays numeric", false, false, 7, ""},
		{`"abc"`, "plain string id stays string", true, false, 0, "abc"},
		{`"1.5"`, "quoted non-integer id stays string", true, false, 0, "1.5"},
	}
	for _, tc := range cases {
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":"m"}`, tc.idJSON)
		frame := fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)
		fr, err := codec.ReadFrame(strings.NewReader(frame))
		if err != nil {
			t.Fatalf("%s: read frame: %v", tc.checkName, err)
		}
		got := fr.Msg.ID
		if got == nil || got.IsStr != tc.isStr || got.IsNull != tc.isNull || got.Num != tc.num || got.Str != tc.str {
			t.Fatalf("%s: decoded %+v", tc.checkName, got)
		}
	}
}
