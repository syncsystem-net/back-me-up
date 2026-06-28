package ratelimit

import (
	"context"
	"testing"
	"time"
)

func TestNilLimiterIsNoOp(t *testing.T) {
	var l *Limiter // nil
	if err := l.WaitRequest(context.Background()); err != nil {
		t.Fatalf("nil WaitRequest: %v", err)
	}
	if err := l.WaitBytes(context.Background(), 1<<30); err != nil {
		t.Fatalf("nil WaitBytes: %v", err)
	}
}

func TestNewReturnsNilWhenUnlimited(t *testing.T) {
	if l := New(0, 0); l != nil {
		t.Fatalf("expected nil limiter when both rates are zero, got %v", l)
	}
}

func TestWaitRequestPacesAfterBurst(t *testing.T) {
	// 10 req/s => burst of 10. The 11th request must wait ~100ms for a refill.
	l := New(10, 0)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		if err := l.WaitRequest(ctx); err != nil {
			t.Fatalf("burst request %d: %v", i, err)
		}
	}
	start := time.Now()
	if err := l.WaitRequest(ctx); err != nil {
		t.Fatalf("post-burst request: %v", err)
	}
	elapsed := time.Since(start)
	// Expect roughly 1/10s; allow generous slack for slow CI.
	if elapsed < 50*time.Millisecond {
		t.Fatalf("expected request to be throttled ~100ms, waited only %v", elapsed)
	}
}

func TestWaitBytesSplitsLargerThanBurst(t *testing.T) {
	// 1000 bytes/s, burst 1000. Requesting 3000 bytes must take ~2s (first 1000
	// from the full bucket, then two more 1000-byte refills).
	l := New(0, 1000)
	start := time.Now()
	if err := l.WaitBytes(context.Background(), 3000); err != nil {
		t.Fatalf("WaitBytes: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 1500*time.Millisecond {
		t.Fatalf("expected ~2s of throttling for 3000 bytes at 1000/s, got %v", elapsed)
	}
}

func TestWaitRespectsContextCancellation(t *testing.T) {
	l := New(0, 1) // 1 byte/s: a large request would block for ages
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := l.WaitBytes(ctx, 1_000_000)
	if err == nil {
		t.Fatal("expected context cancellation error, got nil")
	}
}

func TestSetForUnknownProviderIsNil(t *testing.T) {
	s := NewSet(map[string]*Limiter{"mega": New(5, 0)})
	if s.For("mega") == nil {
		t.Fatal("expected configured limiter for mega")
	}
	if s.For("unknown") != nil {
		t.Fatal("expected nil for unknown provider")
	}
	var nilSet *Set
	if nilSet.For("mega") != nil {
		t.Fatal("expected nil-set lookup to return nil")
	}
}
