package localdaemon

import (
	"context"
	"sync"
	"time"
)

// clientConfigurationTTL bounds how long the alias domain from discovery is reused: every inventory refresh asked
// for it, and it changes only when the operator moves the domain (PR-03).
const clientConfigurationTTL = 30 * time.Minute

// clientConfigurationNow is the cache clock; tests move it.
var clientConfigurationNow = time.Now

// cachedClientConfiguration reads fetch at most once per TTL; a failed read is not cached.
func cachedClientConfiguration[T any](fetch func(context.Context) (T, error), now func() time.Time) func(context.Context) (T, error) {
	var mu sync.Mutex
	var value T
	var at time.Time
	return func(ctx context.Context) (T, error) {
		mu.Lock()
		defer mu.Unlock()
		if !at.IsZero() && now().Sub(at) < clientConfigurationTTL {
			return value, nil
		}
		fetched, err := fetch(ctx)
		if err != nil {
			var zero T
			return zero, err
		}
		value, at = fetched, now()
		return value, nil
	}
}
