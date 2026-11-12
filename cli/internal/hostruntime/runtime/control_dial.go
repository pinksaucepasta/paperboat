//go:build darwin || linux || windows

package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
)

// controlDialer resolves the control host with a bounded retry and remembers
// the last good addresses on disk, so a staged generation still starts when a
// single DNS lookup times out (pbvm4 run14: the staged session owner's first
// authority refresh failed on one DNS timeout and the terminal move was
// skipped). TLS still verifies the hostname; only the address lookup is reused.
type controlDialer struct {
	cache   string // owner-only file in the runtime state root; "" disables reuse
	lookup  func(context.Context, string) ([]net.IPAddr, error)
	dial    func(context.Context, string, string) (net.Conn, error)
	backoff []time.Duration
}

func newControlDialer(stateRoot string) *controlDialer {
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	cache := ""
	if filepath.IsAbs(stateRoot) {
		cache = filepath.Join(stateRoot, "runtime", "control-addresses.json")
	}
	return &controlDialer{cache: cache, lookup: net.DefaultResolver.LookupIPAddr, dial: d.DialContext, backoff: []time.Duration{200 * time.Millisecond, 600 * time.Millisecond}}
}

func (c *controlDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || net.ParseIP(host) != nil {
		return c.dial(ctx, network, address)
	}
	addrs, lookupErr := c.resolve(ctx, host)
	if lookupErr != nil {
		if addrs = c.remembered(host); len(addrs) == 0 {
			return nil, lookupErr
		}
	} else {
		c.remember(host, addrs)
	}
	var dialErr error
	for _, ip := range addrs {
		conn, err := c.dial(ctx, network, net.JoinHostPort(ip, port))
		if err == nil {
			return conn, nil
		}
		dialErr = errors.Join(dialErr, err)
	}
	return nil, errors.Join(lookupErr, dialErr)
}

func (c *controlDialer) resolve(ctx context.Context, host string) ([]string, error) {
	var lastErr error
	for attempt := 0; ; attempt++ {
		found, err := c.lookup(ctx, host)
		if err == nil && len(found) > 0 {
			out := make([]string, 0, len(found))
			for _, a := range found {
				out = append(out, a.IP.String())
			}
			return out, nil
		}
		if err == nil {
			err = &net.DNSError{Err: "no addresses", Name: host, IsNotFound: true}
		}
		lastErr = err
		var dnsErr *net.DNSError
		if attempt >= len(c.backoff) || !errors.As(err, &dnsErr) || dnsErr.IsNotFound || ctx.Err() != nil {
			return nil, lastErr
		}
		select {
		case <-ctx.Done():
			return nil, errors.Join(lastErr, ctx.Err())
		case <-time.After(c.backoff[attempt]):
		}
	}
}

type rememberedAddresses struct {
	Host      string   `json:"host"`
	Addresses []string `json:"addresses"`
	At        int64    `json:"at"`
}

// rememberedAge bounds how stale a remembered control address may be. The cache is only the fallback when DNS fails
// (resolve always runs first, so a changed address is picked up as soon as DNS answers) and TLS verifies the hostname;
// a day covers an outage, a week would pin a moved control plane's old address through a long one.
const rememberedAge = 24 * time.Hour

func (c *controlDialer) remember(host string, addrs []string) {
	if c.cache == "" || len(addrs) > 16 {
		return
	}
	body, err := json.Marshal(rememberedAddresses{Host: host, Addresses: addrs, At: time.Now().Unix()})
	if err != nil {
		return
	}
	// Atomic and owner-only; a link planted at the path is refused, never written through.
	_ = atomicfile.Write(c.cache, body, atomicfile.Options{Mode: 0o600, OwnerUID: -1, OwnerGID: -1})
}

func (c *controlDialer) remembered(host string) []string {
	if c.cache == "" {
		return nil
	}
	// Only a regular file this account owns and nobody else can touch is read: a link, or a file another account
	// dropped there, could point the control host at addresses nobody here wrote. TLS still verifies the hostname,
	// so a stale entry can only fail to connect.
	if info, err := os.Lstat(c.cache); err != nil || !info.Mode().IsRegular() || !cacheFileTrusted(c.cache, info) {
		return nil
	}
	body, err := os.ReadFile(c.cache)
	if err != nil || len(body) > 4096 {
		return nil
	}
	var r rememberedAddresses
	if json.Unmarshal(body, &r) != nil || r.Host != host || controlAddressStale(r.At) {
		return nil
	}
	var out []string
	for _, a := range r.Addresses {
		if net.ParseIP(a) != nil {
			out = append(out, a)
		}
	}
	return out
}

func controlAddressStale(at int64) bool { return time.Since(time.Unix(at, 0)) > rememberedAge }
