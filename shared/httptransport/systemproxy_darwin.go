//go:build darwin

package httptransport

import (
	"context"
	"fmt"
	"os/exec"
	"sync"
	"time"
)

// scutilCacheTTL bounds how stale a proxy change can be; RoundTrip asks for a snapshot on every request and each
// miss is a process spawn.
const scutilCacheTTL = 5 * time.Second

var scutilCache struct {
	sync.Mutex
	at       time.Time
	snapshot ProxySnapshot
}

type NativeSystemProxySource struct{}

func (NativeSystemProxySource) Snapshot(ctx context.Context) (ProxySnapshot, error) {
	scutilCache.Lock()
	defer scutilCache.Unlock()
	if !scutilCache.at.IsZero() && time.Since(scutilCache.at) < scutilCacheTTL {
		return scutilCache.snapshot, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/scutil", "--proxy").Output()
	if err != nil {
		return ProxySnapshot{}, fmt.Errorf("read macOS system proxy with scutil --proxy: %w", err)
	}
	scutilCache.snapshot, scutilCache.at = parseScutilProxy(string(out)), time.Now()
	return scutilCache.snapshot, nil
}
