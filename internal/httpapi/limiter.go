package httpapi

import (
	"sync"
	"time"
)

// failureLimiter blocks a key after max failures inside a sliding window. It guards
// the 6-digit device PIN against brute force. State is in memory, so it resets on
// restart and is per instance; fine for a single Go service.
type failureLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	now    func() time.Time
	fails  map[string][]time.Time
}

func newFailureLimiter(max int, window time.Duration) *failureLimiter {
	return &failureLimiter{max: max, window: window, now: time.Now, fails: make(map[string][]time.Time)}
}

// prune drops failures older than the window. Callers must hold l.mu.
func (l *failureLimiter) prune(key string) []time.Time {
	cutoff := l.now().Add(-l.window)
	recent := l.fails[key][:0]
	for _, t := range l.fails[key] {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}
	if len(recent) == 0 {
		delete(l.fails, key)
		return nil
	}
	l.fails[key] = recent
	return recent
}

// allow reports whether key may try again, and if not, how long to wait.
func (l *failureLimiter) allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	recent := l.prune(key)
	if len(recent) < l.max {
		return true, 0
	}
	return false, recent[0].Add(l.window).Sub(l.now())
}

func (l *failureLimiter) fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.prune(key)
	l.fails[key] = append(l.fails[key], l.now())
}
