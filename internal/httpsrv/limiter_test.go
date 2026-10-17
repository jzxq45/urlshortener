package httpsrv

import (
	"fmt"
	"testing"
	"time"
)

func TestTokenLimiterBurstAndRefill(t *testing.T) {
	t.Parallel()
	l := newTokenLimiter(3)
	// fresh key starts full: three passes, then blocked (sequential calls —
	// staticcheck SA4000 flags the chained identical-expression form)
	if !l.Allow("a") {
		t.Fatal("first allowance consumed")
	}
	if !l.Allow("a") {
		t.Fatal("second allowance consumed")
	}
	if !l.Allow("a") {
		t.Fatal("third allowance consumed")
	}
	if l.Allow("a") {
		t.Error("4th call should be blocked")
	}

	// independent key unaffected
	if !l.Allow("b") {
		t.Error("new key should start full")
	}

	// simulated refill: force an old timestamp so elapsed time grants tokens
	l.mu.Lock()
	l.buckets["a"].last = time.Now().Add(-time.Minute)
	l.mu.Unlock()
	if !l.Allow("a") {
		t.Error("minute of refill should grant one token")
	}
}

func TestTokenLimiterPrune(t *testing.T) {
	t.Parallel()
	l := newTokenLimiter(1)
	for i := 0; i < 4096; i++ {
		l.Allow(fmt.Sprintf("k%d", i))
	}
	// make all buckets idle for longer than the prune window and let the
	// next allowance pass trigger the (stale) janitor
	l.mu.Lock()
	now := time.Now()
	for _, b := range l.buckets {
		b.last = now.Add(-10 * time.Minute)
	}
	l.prunedAt = now.Add(-2 * time.Minute)
	l.mu.Unlock()
	l.Allow("catalyst")

	l.mu.Lock()
	n := len(l.buckets)
	l.mu.Unlock()
	if n > 2 {
		t.Errorf("buckets not pruned: %d", n)
	}
}
