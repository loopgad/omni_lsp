package httpserver

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestPositionEndpointsBoundRequestBody pins bug round 20 A#16 (圈 5): the
// /api/v1/hover|definition|references endpoints decoded r.Body with no byte
// cap, so one huge JSON string value ({"uri":"<gigabytes>"}) was materialized
// whole by json.Decoder. Loopback-only (N8) plus optional bearer auth bounds
// who can send it, not what it costs; the endpoints need a URI and two small
// integers, so the body is capped and an over-limit request is answered 413
// before any materialization. Legitimate requests keep working.
func TestPositionEndpointsBoundRequestBody(t *testing.T) {
	fc := &fakeCore{ready: true}
	ts := newTestServer(fc, Options{})
	defer ts.Close()

	huge := strings.Repeat("a", (1<<20)+64)
	oversized := fmt.Sprintf(`{"uri":"file:///w/%s.go","line":1,"column":0}`, huge)
	resp := postJSON(t, ts.URL+"/api/v1/hover", oversized)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body status = %d, want 413 (the body must be capped before json.Decoder materializes it)", resp.StatusCode)
	}

	// A legitimate request still decodes and reaches the core.
	resp2 := postJSON(t, ts.URL+"/api/v1/hover", `{"uri":"file:///w/a.go","line":0,"column":0}`)
	defer resp2.Body.Close()
	if resp2.StatusCode == http.StatusRequestEntityTooLarge {
		t.Fatal("legitimate request was rejected as oversized")
	}
	if resp2.StatusCode >= 500 {
		t.Fatalf("legitimate request failed server-side: %d", resp2.StatusCode)
	}
}
