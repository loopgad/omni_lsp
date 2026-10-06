package httpserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func stubCoreForAuth() Core { return &fakeCore{} }

func TestX6_BearerTokenGate(t *testing.T) {
	srv := New(stubCoreForAuth(), Options{
		Addr: "127.0.0.1:0",
		Auth: AuthOptions{Tokens: map[string]bool{"secret": true}},
	})
	do := func(tok string) int {
		req := httptest.NewRequest("POST", "/api/v1/hover", strings.NewReader(`{"uri":"file:///w/a.go","line":0,"column":0}`))
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := do("secret"); code == http.StatusUnauthorized {
		t.Errorf("valid token rejected: %d", code)
	}
	if code := do("wrong"); code != http.StatusUnauthorized {
		t.Errorf("invalid token = %d, want 401", code)
	}
	if code := do(""); code != http.StatusUnauthorized {
		t.Errorf("missing token = %d, want 401", code)
	}

	// Local trust mode: empty token set allow-alls (ADR-0007).
	open := New(stubCoreForAuth(), Options{Addr: "127.0.0.1:0"})
	req := httptest.NewRequest("GET", "/health", nil)
	rec := httptest.NewRecorder()
	open.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("local-trust health = %d, want 200", rec.Code)
	}
}

func TestX6_PerSourceRateLimitBucket(t *testing.T) {
	srv := New(stubCoreForAuth(), Options{
		Addr: "127.0.0.1:0",
		Auth: AuthOptions{Tokens: map[string]bool{"t": true}, PerSourcePerMinute: 3},
	})
	do := func() int {
		req := httptest.NewRequest("POST", "/api/v1/hover", strings.NewReader(`{"uri":"file:///w/a.go","line":0,"column":0}`))
		req.Header.Set("Authorization", "Bearer t")
		req.RemoteAddr = "10.0.0.1:1234"
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec.Code
	}
	for i := 0; i < 3; i++ {
		if code := do(); code >= 429 {
			t.Fatalf("request %d limited early: %d", i, code)
		}
	}
	if code := do(); code != http.StatusTooManyRequests {
		t.Errorf("4th request = %d, want 429", code)
	}

	// Different source keeps its own bucket (§F6-style isolation: bounded work per key).
	req := httptest.NewRequest("POST", "/api/v1/hover", strings.NewReader(`{"uri":"file:///w/a.go","line":0,"column":0}`))
	req.Header.Set("Authorization", "Bearer other-token")
	req.RemoteAddr = "10.0.0.2:1234"
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code >= 429 {
		t.Errorf("independent source limited: %d", rec.Code)
	}
}
