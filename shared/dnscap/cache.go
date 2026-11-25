package dnscap

import (
	"context"
	"encoding/json"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// The address each fixed Paperboat host last answered TLS at, after a lookup (Tailscale net/dnscache). It is dialed at
// once for a day; from 10 minutes on a dial also starts one lookup in the background. TLS stays the authority: only an
// address whose handshake verified the host name is kept.
const (
	freshFor    = 10 * time.Minute
	lastGoodFor = 24 * time.Hour
	retryAfter  = time.Minute // between background lookups of one host
	maxHosts    = 32
	// KeptDialWait bounds a dial of a kept address: a host that moved is then looked up.
	KeptDialWait = 3 * time.Second
)

type entry struct {
	Address   netip.Addr `json:"address"`
	Verified  time.Time  `json:"verified"`
	refreshed time.Time
}

var (
	mu     sync.Mutex
	saving sync.Mutex // held over a write to disk, which must not hold up a dial that only reads
	hosts  = map[string]entry{}
	save   func([]byte)
)

// Cached is the kept address of host; invalid: none.
func Cached(host string) netip.Addr {
	mu.Lock()
	defer mu.Unlock()
	e, ok := hosts[strings.ToLower(host)]
	if age := time.Since(e.Verified); !ok || age < 0 || age > lastGoodFor {
		return netip.Addr{}
	}
	return e.Address
}

// Refresh runs dial, which looks host up and calls Verified, in the background if the kept address is due for it.
func Refresh(host string, dial func(context.Context)) {
	host = strings.ToLower(host)
	mu.Lock()
	defer mu.Unlock()
	e, ok := hosts[host]
	if !ok || time.Since(e.Verified) < freshFor || time.Since(e.refreshed) < retryAfter {
		return
	}
	e.refreshed = time.Now()
	hosts[host] = e
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), retryAfter/2)
		defer cancel()
		dial(ctx)
	}()
}

// Verified keeps the looked-up address a TLS handshake just verified as host. A private address (a captive portal, a company's
// own network) is used but not kept.
func Verified(host string, remote net.Addr) {
	endpoint, err := netip.ParseAddrPort(remote.String())
	address, host := endpoint.Addr().Unmap(), strings.ToLower(host)
	if err != nil || !cacheable(host, address) {
		return
	}
	mu.Lock()
	old, known := hosts[host]
	if old.Address == address && time.Since(old.Verified) < time.Minute {
		mu.Unlock()
		return
	}
	if !known && len(hosts) >= maxHosts { // full: the oldest goes
		oldest := ""
		for name, e := range hosts {
			if oldest == "" || e.Verified.Before(hosts[oldest].Verified) {
				oldest = name
			}
		}
		delete(hosts, oldest)
	}
	hosts[host] = entry{Address: address, Verified: time.Now()}
	mu.Unlock()
	saving.Lock()
	defer saving.Unlock()
	mu.Lock()
	body, err := json.Marshal(hosts)
	write := save
	mu.Unlock()
	if err == nil && write != nil {
		write(body) // shortcut: the last process to write wins, another's newer host is verified and written again
	}
}

// Persist loads what an earlier process verified and hands every later change to write.
func Persist(body []byte, write func([]byte)) {
	loaded := map[string]entry{}
	if json.Unmarshal(body, &loaded) != nil || len(loaded) > maxHosts {
		loaded = nil
	}
	mu.Lock()
	defer mu.Unlock()
	save = write
	for host, e := range loaded {
		if old, known := hosts[host]; cacheable(host, e.Address) && (!known || e.Verified.After(old.Verified)) {
			hosts[host] = e
		}
	}
}

func cacheable(host string, address netip.Addr) bool {
	_, literal := netip.ParseAddr(host)
	return host != "" && literal != nil && address.IsGlobalUnicast() && !address.IsPrivate()
}
