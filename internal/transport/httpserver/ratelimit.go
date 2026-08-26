package httpserver

import (
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// tokenBucket is a global rate limiter: one bucket shared by all clients.
// ponytail: global bucket; switch to per-IP buckets if multi-client fairness matters.
type tokenBucket struct {
	mu     sync.Mutex
	tokens float64
	burst  float64
	rate   float64 // tokens added per second
	last   time.Time
}

func newTokenBucket(rate float64, burst int) *tokenBucket {
	return &tokenBucket{
		tokens: float64(burst),
		burst:  float64(burst),
		rate:   rate,
		last:   time.Now(),
	}
}

// allow consumes one token if available; when denied it returns how long
// until the next token becomes available.
func (b *tokenBucket) allow(now time.Time) (bool, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tokens = min(b.burst, b.tokens+now.Sub(b.last).Seconds()*b.rate)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration(math.Ceil((1-b.tokens)/b.rate) * float64(time.Second))
	return false, wait
}

// limit returns a handler enforcing the bucket on the wrapped route,
// answering 429 + Retry-After when exhausted.
func (s *Server) limit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok, wait := s.bucket.allow(time.Now())
		if !ok {
			w.Header().Set("Retry-After", strconv.Itoa(max(1, int(wait.Seconds()))))
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}
