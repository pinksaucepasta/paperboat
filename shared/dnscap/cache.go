package dnscap

import (
	"encoding/json"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// The address each fixed Paperboat host last answered TLS at (Tailscale net/dnscache: 10 minutes, then last good for
// at most 3 s of looking up). TLS stays the authority: only an address whose handshake verified the host name is kept.
const (
	freshFor    = 10 * time.Minute
	lastGoodFor = 24 * time.Hour
	// LastGoodWait bounds a dial of a kept address, and a lookup while there is one to fall back to.
	LastGoodWait = 3 * time.Second
	maxHosts     = 32
)

type entry struct {
	Address  netip.Addr `json:"address"`
	Verified time.Time  `json:"verified"`
	failed   bool       // a dial failed since: looked up again, kept as last good
}

var (
	mu     sync.Mutex
	saving sync.Mutex // held over a write to disk, which must not hold up a dial that only reads
	hosts  = map[string]entry{}
	save   func([]byte)
)

// Cached is the kept address of host (invalid: none); fresh ones are used without a lookup.
func Cached(host string) (address netip.Addr, fresh bool) {
	mu.Lock()
	defer mu.Unlock()
	e, ok := hosts[strings.ToLower(host)]
	age := time.Since(e.Verified)
	if !ok || age < 0 || age > lastGoodFor {
		return netip.Addr{}, false
	}
	return e.Address, age < freshFor && !e.failed
}

// Verified keeps the address a TLS handshake just verified as host. A private address (a captive portal, a company's
// own network) is used but not kept.
func Verified(host string, remote net.Addr) {
	endpoint, err := netip.ParseAddrPort(remote.String())
	address, host := endpoint.Addr().Unmap(), strings.ToLower(host)
	if err != nil || !cacheable(host, address) {
		return
	}
	mu.Lock()
	old, known := hosts[host]
	if !known && len(hosts) >= maxHosts || known && old.Address == address && !old.failed && time.Since(old.Verified) < time.Minute {
		mu.Unlock()
		return
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

// Expire makes the next dial of host look it up, if address is still the kept one.
func Expire(host string, address netip.Addr) {
	mu.Lock()
	defer mu.Unlock()
	if e, ok := hosts[strings.ToLower(host)]; ok && e.Address == address {
		e.failed = true
		hosts[strings.ToLower(host)] = e
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
