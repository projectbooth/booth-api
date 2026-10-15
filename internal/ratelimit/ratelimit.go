// Package ratelimit is booth-api's per-principal write rate limit (ADR 0117 "Reconciled design"
// item 5): a token bucket per principal, per pod, 10 writes a second with a burst of 50 by
// default. A write loop fills a database; reads stay unlimited (ADR 0100 deferred that).
package ratelimit

import (
	"math"
	"sync"
	"time"
)

// Limiter holds one bucket per key.
type Limiter struct {
	Rate  float64 // tokens per second
	Burst float64
	Now   func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
	swept   time.Time
}

type bucket struct {
	tokens float64
	at     time.Time
}

// New returns a limiter allowing rate per second with the given burst.
func New(rate float64, burst int) *Limiter {
	return &Limiter{Rate: rate, Burst: float64(burst), Now: time.Now, buckets: map[string]*bucket{}}
}

// Allow takes one token from key's bucket. If none is left it returns false and how long until
// one will be.
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.Now()
	l.sweep(now)
	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: l.Burst, at: now}
		l.buckets[key] = b
	}
	b.tokens = math.Min(l.Burst, b.tokens+now.Sub(b.at).Seconds()*l.Rate)
	b.at = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) / l.Rate * float64(time.Second))
	return false, wait
}

// sweep drops full buckets once a minute, so the map holds only recently active principals.
func (l *Limiter) sweep(now time.Time) {
	if now.Sub(l.swept) < time.Minute {
		return
	}
	l.swept = now
	for k, b := range l.buckets {
		if b.tokens+now.Sub(b.at).Seconds()*l.Rate >= l.Burst {
			delete(l.buckets, k)
		}
	}
}
