package httpsrv

import (
	"sync"
	"time"
)

// tokenLimiter is a per-key token bucket with continuous refill. Buckets are
// pruned lazily on access so no janitor goroutine survives shutdown.
type tokenLimiter struct {
	capacity int        // == per-minute allowance
	rate     float64    // tokens per second
	mu       sync.Mutex // serializes bucket map access
	buckets  map[string]*tbucket
	prunedAt time.Time
}

// tbucket stays cheap: refill is embedded in Allow so no timers per key.
type tbucket struct {
	tokens float64
	last   time.Time
}

func newTokenLimiter(perMinute int) *tokenLimiter {
	return &tokenLimiter{
		capacity: perMinute,
		rate:     float64(perMinute) / 60.0,
		buckets:  make(map[string]*tbucket),
	}
}

// Allow spends one token if available.
func (l *tokenLimiter) Allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	if b, ok := l.buckets[key]; ok {
		b.tokens = minFloat(float64(l.capacity), b.tokens+now.Sub(b.last).Seconds()*l.rate)
		b.last = now
		if b.tokens >= 1 {
			b.tokens -= 1
			return true
		}
		return false
	}
	// new keys start full so a fresh client can burst its allowance
	l.buckets[key] = &tbucket{tokens: float64(l.capacity) - 1, last: now}
	if len(l.buckets) > 4096 && now.Sub(l.prunedAt) > time.Minute {
		l.prune(now)
		l.prunedAt = now
	}
	return true
}

// prune drops buckets idle over two refill windows.
func (l *tokenLimiter) prune(now time.Time) {
	for k, b := range l.buckets {
		if now.Sub(b.last) > 2*time.Minute {
			delete(l.buckets, k)
		}
	}
}

func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
