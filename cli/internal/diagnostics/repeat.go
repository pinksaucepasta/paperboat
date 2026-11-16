package diagnostics

import (
	"strings"
	"sync"
	"time"
)

// repeatWindow is how long a repeat of one failure from one call site is left out of the record. About fifteen runtime
// loops each retry with their own backoff when the control plane refuses a revoked machine; every attempt wrote an event
// (X-41: 80-98 in the first minute). The first occurrence is always recorded.
const repeatWindow = 30 * time.Second

// repeatClassCap bounds the map: with this many live classes a new class is never suppressed.
const repeatClassCap = 512

type repeatLimiter struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// repeatClass names a record by where it came from and what it says, not by its support reference or time.
func repeatClass(category, code, severity string, fields map[string]string) string {
	return strings.Join([]string{category, code, severity, fields["component"], fields["operation"], fields["source_function"],
		fields["cause"], fields["outcome"], fields["errno"], fields["http_status"]}, "|")
}

func (l *repeatLimiter) suppress(class string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if last, seen := l.last[class]; seen && now.Sub(last) < repeatWindow {
		return true
	}
	if l.last == nil {
		l.last = map[string]time.Time{}
	}
	if len(l.last) >= repeatClassCap {
		for key, at := range l.last {
			if now.Sub(at) >= repeatWindow {
				delete(l.last, key)
			}
		}
	}
	if len(l.last) < repeatClassCap {
		l.last[class] = now
	}
	return false
}
