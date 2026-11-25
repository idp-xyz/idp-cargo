package mcp

import (
	"sync"
	"time"
)

// Limits are the rates one caller may use (idp-web D22): calls in a sliding minute and calls at once.
type Limits struct {
	PerMinute  int
	Concurrent int
}

// Default limits.
const (
	DefaultPerMinute  = 60
	DefaultConcurrent = 4
)

// limiter keeps, in memory and for this process only, when each caller called in the last minute and how many of its
// calls run now.
type limiter struct {
	limits Limits
	now    func() time.Time

	mu     sync.Mutex
	recent map[string][]time.Time
	active map[string]int
}

func newLimiter(l Limits, now func() time.Time) *limiter {
	return &limiter{limits: l, now: now, recent: map[string][]time.Time{}, active: map[string]int{}}
}

// take admits a call of caller (a key made of the tenant and the subject) or reports that the caller is over a limit.
// The returned func ends the call.
func (l *limiter) take(caller string) (func(), bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.forget(now)
	if l.active[caller] >= l.limits.Concurrent || len(l.recent[caller]) >= l.limits.PerMinute {
		return nil, false
	}
	l.recent[caller] = append(l.recent[caller], now)
	l.active[caller]++
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.active[caller]--; l.active[caller] <= 0 {
			delete(l.active, caller)
		}
	}, true
}

// forget drops the calls older than a minute, and the callers with none, so that the table stays as small as the
// callers of the last minute.
func (l *limiter) forget(now time.Time) {
	cut := now.Add(-time.Minute)
	for k, calls := range l.recent {
		i := 0
		for i < len(calls) && !calls[i].After(cut) {
			i++
		}
		if i == len(calls) {
			delete(l.recent, k)
		} else if i > 0 {
			l.recent[k] = calls[i:]
		}
	}
}
