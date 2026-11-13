package route

import (
	"strings"
	"sync"
)

// UnavailableHosts is the set of public hostnames whose route the server assigned to this edge but
// whose carrier group failed its probe. The TLS selector and the HTTP policy read it so a visitor gets
// a handshake and an error page instead of a dropped connection.
type UnavailableHosts struct {
	mu    sync.RWMutex
	hosts map[string]struct{}
}

// Replace swaps the whole set; a nil or empty list clears it.
func (u *UnavailableHosts) Replace(hosts []string) {
	next := make(map[string]struct{}, len(hosts))
	for _, h := range hosts {
		if h = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), ".")); h != "" {
			next[h] = struct{}{}
		}
	}
	u.mu.Lock()
	u.hosts = next
	u.mu.Unlock()
}

func (u *UnavailableHosts) Has(host string) bool {
	if u == nil {
		return false
	}
	u.mu.RLock()
	defer u.mu.RUnlock()
	_, ok := u.hosts[strings.ToLower(strings.TrimSuffix(host, "."))]
	return ok
}
