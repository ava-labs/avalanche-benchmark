package gateway

import (
	"sync"
	"time"
)

type fixedWindowLimiter struct {
	mu      sync.Mutex
	windows map[string]windowState
}

type windowState struct {
	windowStart time.Time
	count       int
}

func newFixedWindowLimiter() *fixedWindowLimiter {
	return &fixedWindowLimiter{
		windows: make(map[string]windowState),
	}
}

func (l *fixedWindowLimiter) Allow(key string, limit int, now time.Time) bool {
	if limit <= 0 {
		return true
	}

	windowStart := now.UTC().Truncate(time.Minute)

	l.mu.Lock()
	defer l.mu.Unlock()

	state, ok := l.windows[key]
	if !ok || !state.windowStart.Equal(windowStart) {
		l.windows[key] = windowState{
			windowStart: windowStart,
			count:       1,
		}
		return true
	}

	if state.count >= limit {
		return false
	}

	state.count++
	l.windows[key] = state
	return true
}
