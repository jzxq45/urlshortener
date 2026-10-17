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

// maxBuckets caps memory under attack-sized cardinality; the prune trigger
// fires at 2x so the O(n) sweep amortizes to O(1) per accepted insert.
const maxBuckets = 4096

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
	// trigger is 2x the target size: between sweeps up to maxBuckets fresh
	// keys pay O(1), so the full scan never rides a request hotspot
	if len(l.buckets) >= 2*maxBuckets && now.Sub(l.prunedAt) > time.Minute {
		l.prune(now)
		l.prunedAt = now
	}
	return true
}

// prune drops already-expired buckets until the map is back at maxBuckets;
// running to the target (not to zero) is what makes it amortized O(1).
func (l *tokenLimiter) prune(now time.Time) {
	for k, b := range l.buckets {
		if len(l.buckets) <= maxBuckets {
			return
		}
		// idle over two refill windows = unusable anyway, safe to drop
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
