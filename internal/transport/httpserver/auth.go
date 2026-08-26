package httpserver

import (
	"net/http"
	"strings"
	"sync"
)

// AuthOptions gates the read API (§X6/C15/N8). Zero value = local trust
// mode: every request passes, matching the trust package's
// implicit-local-launch decision (ADR-0007). Any configured token switches
// the surface to bearer authentication.
type AuthOptions struct {
	// Tokens is the set of accepted bearer tokens. Empty = allow-all.
	Tokens map[string]bool

	// PerSourcePerMinute caps requests per source key (remote IP or token).
	// Zero disables per-source limiting.
	PerSourcePerMinute int
}

// sourceKey identifies a rate-limit bucket: the token when authenticated,
// else the remote address.
func sourceKey(r *http.Request, tok string) string {
	if tok != "" {
		return "tok:" + tok
	}
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	return "ip:" + host
}

// bearerToken extracts the Authorization: Bearer <t> value.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return ""
	}
	return strings.TrimPrefix(h, "Bearer ")
}

// perSourceLimiter is a fixed-window counter per source key. Ponytail
// simple: exact enough for abuse damping, no dependency on x/time/rate.
type perSourceLimiter struct {
	mu      sync.Mutex
	window  int64 // seconds
	limit   int
	buckets map[string]*srcBucket
}

type srcBucket struct {
	count int
	slot  int64
}

func newPerSourceLimiter(perMinute int) *perSourceLimiter {
	return &perSourceLimiter{limit: perMinute, buckets: map[string]*srcBucket{}}
}

func (p *perSourceLimiter) Allow(key string, nowSlot int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	b := p.buckets[key]
	if b == nil || b.slot != nowSlot {
		if len(p.buckets) > 4096 { // bounded cardinality (§P3)
			p.buckets = map[string]*srcBucket{}
		}
		p.buckets[key] = &srcBucket{slot: nowSlot}
		b = p.buckets[key]
	}
	b.slot = nowSlot
	b.count++
	return b.count <= p.limit
}

// withAuth wraps next with bearer-token checking and per-source limiting.
func withAuth(next http.Handler, opts AuthOptions, lim *perSourceLimiter, now func() int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := bearerToken(r)
		if len(opts.Tokens) > 0 && !opts.Tokens[tok] {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if opts.PerSourcePerMinute > 0 && lim != nil {
			if !lim.Allow(sourceKey(r, tok), now()/60) {
				http.Error(w, `{"error":"rate_limited"}`, http.StatusTooManyRequests)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
