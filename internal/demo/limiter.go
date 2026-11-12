package demo

import (
	"sync"
	"time"
)

// Limiter allows at most n hits per key in any window of length per, a
// sliding log. Demo mode is one process, so the log lives in memory: nothing
// about visitors is stored, and a restart forgets it.
type Limiter struct {
	n   int
	per time.Duration
	now func() time.Time

	mu        sync.Mutex
	hits      map[string][]time.Time
	lastSweep time.Time
}

// NewLimiter takes the clock as a function so tests can move time.
func NewLimiter(n int, per time.Duration, now func() time.Time) *Limiter {
	return &Limiter{n: n, per: per, now: now, hits: map[string][]time.Time{}}
}

// Allow records a hit for key if it is under the limit. If not, it reports
// how long until the oldest hit leaves the window.
func (l *Limiter) Allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweep(now)
	hits := inWindow(l.hits[key], now.Add(-l.per))
	if len(hits) >= l.n {
		l.hits[key] = hits
		return false, hits[0].Add(l.per).Sub(now)
	}
	l.hits[key] = append(hits, now)
	return true, 0
}

// sweep forgets keys with no hits left in the window, at most once a window,
// so memory follows the visitors of the last hour rather than all of them.
func (l *Limiter) sweep(now time.Time) {
	if now.Sub(l.lastSweep) < l.per {
		return
	}
	l.lastSweep = now
	cutoff := now.Add(-l.per)
	for key, hits := range l.hits {
		if len(inWindow(hits, cutoff)) == 0 {
			delete(l.hits, key)
		}
	}
}

// inWindow drops the hits at or before cutoff; hits are in time order.
func inWindow(hits []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(hits) && !hits[i].After(cutoff) {
		i++
	}
	return hits[i:]
}
