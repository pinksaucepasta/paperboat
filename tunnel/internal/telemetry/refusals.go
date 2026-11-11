package telemetry

import "sync"

// maxPendingRefusalHosts bounds the hostnames counted between heartbeats; a flood across more names
// than this still gets refused, only the owner signal for the extra names is skipped.
const maxPendingRefusalHosts = 256

var pendingRefusals struct {
	mu    sync.Mutex
	hosts map[string]uint64
}

// NoteRefusal counts one request or connection to hostname the edge refused for flood protection
// (a client address over its limit, or the name over its share of the edge), so its owner learns it.
func NoteRefusal(hostname string) {
	if hostname == "" {
		return
	}
	pendingRefusals.mu.Lock()
	defer pendingRefusals.mu.Unlock()
	if pendingRefusals.hosts == nil {
		pendingRefusals.hosts = make(map[string]uint64)
	}
	if _, counted := pendingRefusals.hosts[hostname]; counted || len(pendingRefusals.hosts) < maxPendingRefusalHosts {
		pendingRefusals.hosts[hostname]++
	}
}

// TakeRefusals returns and forgets the counts noted since the last call (the heartbeat sends them).
func TakeRefusals() map[string]uint64 {
	pendingRefusals.mu.Lock()
	defer pendingRefusals.mu.Unlock()
	hosts := pendingRefusals.hosts
	pendingRefusals.hosts = nil
	return hosts
}
