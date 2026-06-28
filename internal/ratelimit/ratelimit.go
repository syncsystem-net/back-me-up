// Package ratelimit provides per-provider request-rate and bandwidth throttling
// for cloud uploads. It is a small self-contained token-bucket implementation so
// the project takes on no extra dependency.
//
// Limiters are created once at startup from config (one per provider) and
// installed as a process-global Set via Configure; the provider registry then
// looks one up by name with For. The limits are intentionally per-provider, not
// per-account, matching the spec's rate_limits.<provider> shape — every account
// of a provider draws from the same budget.
//
// A zero rate disables that dimension, and a nil *Limiter is a valid no-op, so
// callers never need to guard against missing configuration.
package ratelimit

import (
	"context"
	"sync"
	"time"
)

// Limiter enforces a request-per-second ceiling and a bytes-per-second ceiling
// independently. Either dimension is skipped when its rate is zero. A nil
// *Limiter performs no limiting at all.
type Limiter struct {
	req *bucket // tokens measured in requests
	bw  *bucket // tokens measured in bytes
}

// New builds a Limiter. requestsPerSec/bytesPerSec of zero (or negative) leave
// that dimension unlimited. When both are unlimited, New returns nil so the
// caller stores a cheap no-op.
func New(requestsPerSec, bytesPerSec float64) *Limiter {
	if requestsPerSec <= 0 && bytesPerSec <= 0 {
		return nil
	}
	l := &Limiter{}
	if requestsPerSec > 0 {
		// Burst of one second's worth of requests (at least one) so a short idle
		// period lets a small flurry through without forcing serial spacing.
		burst := requestsPerSec
		if burst < 1 {
			burst = 1
		}
		l.req = newBucket(requestsPerSec, burst)
	}
	if bytesPerSec > 0 {
		// Burst of one second of bandwidth; WaitBytes splits larger requests into
		// burst-sized pieces so a chunk far bigger than the burst still smooths out
		// instead of erroring.
		l.bw = newBucket(bytesPerSec, bytesPerSec)
	}
	return l
}

// WaitRequest blocks until one request token is available or ctx is cancelled.
func (l *Limiter) WaitRequest(ctx context.Context) error {
	if l == nil || l.req == nil {
		return nil
	}
	return l.req.wait(ctx, 1)
}

// WaitBytes blocks until n bytes of bandwidth budget are available or ctx is
// cancelled. Requests larger than the burst are consumed in burst-sized pieces.
func (l *Limiter) WaitBytes(ctx context.Context, n int) error {
	if l == nil || l.bw == nil || n <= 0 {
		return nil
	}
	remaining := float64(n)
	for remaining > 0 {
		take := remaining
		if take > l.bw.burst {
			take = l.bw.burst
		}
		if err := l.bw.wait(ctx, take); err != nil {
			return err
		}
		remaining -= take
	}
	return nil
}

// bucket is a classic token bucket: tokens refill continuously at rate per
// second up to burst, and wait removes tokens (sleeping for the shortfall).
type bucket struct {
	rate  float64 // tokens added per second
	burst float64 // maximum accumulated tokens

	mu     sync.Mutex
	tokens float64
	last   time.Time
}

func newBucket(rate, burst float64) *bucket {
	return &bucket{rate: rate, burst: burst, tokens: burst, last: time.Now()}
}

// wait reserves n tokens, returning after enough have accrued. n must not exceed
// burst (callers split larger requests). It respects ctx cancellation.
func (b *bucket) wait(ctx context.Context, n float64) error {
	for {
		b.mu.Lock()
		now := time.Now()
		b.tokens += now.Sub(b.last).Seconds() * b.rate
		if b.tokens > b.burst {
			b.tokens = b.burst
		}
		b.last = now
		if b.tokens >= n {
			b.tokens -= n
			b.mu.Unlock()
			return nil
		}
		// Sleep for the time needed to accumulate the shortfall.
		deficit := n - b.tokens
		wait := time.Duration(deficit / b.rate * float64(time.Second))
		b.mu.Unlock()

		if wait <= 0 {
			wait = time.Millisecond
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// Set maps a provider name to its Limiter. A missing entry yields a nil Limiter
// (no limiting), which is a valid value everywhere.
type Set struct {
	m map[string]*Limiter
}

// NewSet builds a Set from a name->Limiter map.
func NewSet(m map[string]*Limiter) *Set {
	if m == nil {
		m = map[string]*Limiter{}
	}
	return &Set{m: m}
}

// For returns the Limiter registered for name, or nil (no limiting).
func (s *Set) For(name string) *Limiter {
	if s == nil {
		return nil
	}
	return s.m[name]
}

// global is the process-wide Set installed by Configure and read by the
// package-level For. It lets the registry attach a limiter without threading a
// Set through cloud.Connect and every handler.
var global = NewSet(nil)

// Configure installs the process-global Set. Call once at startup before any
// uploads begin.
func Configure(s *Set) {
	if s == nil {
		s = NewSet(nil)
	}
	global = s
}

// For returns the process-global Limiter for a provider name, or nil.
func For(name string) *Limiter { return global.For(name) }
