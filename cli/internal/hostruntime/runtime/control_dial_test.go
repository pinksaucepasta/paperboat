//go:build darwin || linux || windows

package runtime

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// pbvm4 run14: one DNS timeout stopped the staged carrier from starting.
func TestControlDialSurvivesOneDNSTimeoutAndReusesAddresses(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	root := t.TempDir()
	d := newControlDialer(root)
	d.cache = filepath.Join(root, "control-addresses.json")
	d.backoff = []time.Duration{time.Millisecond, time.Millisecond}
	calls := 0
	d.lookup = func(context.Context, string) ([]net.IPAddr, error) {
		calls++
		if calls == 1 {
			return nil, &net.DNSError{Err: "i/o timeout", Name: "api.example.test", IsTimeout: true}
		}
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	}
	conn, err := d.DialContext(t.Context(), "tcp", net.JoinHostPort("api.example.test", port))
	if err != nil || calls != 2 {
		t.Fatalf("one DNS timeout was not retried: calls=%d err=%v", calls, err)
	}
	conn.Close()
	// DNS now fails every time: the address a previous lookup remembered is used.
	d.lookup = func(context.Context, string) ([]net.IPAddr, error) {
		return nil, &net.DNSError{Err: "i/o timeout", Name: "api.example.test", IsTimeout: true}
	}
	fresh := newControlDialer(root)
	fresh.cache, fresh.lookup, fresh.backoff = d.cache, d.lookup, d.backoff
	conn, err = fresh.DialContext(t.Context(), "tcp", net.JoinHostPort("api.example.test", port))
	if err != nil {
		t.Fatal("remembered control address was not reused after DNS failed:", err)
	}
	conn.Close()
	// A definite NXDOMAIN is not retried and a remembered address of another host is not used.
	calls = 0
	fresh.lookup = func(context.Context, string) ([]net.IPAddr, error) {
		calls++
		return nil, &net.DNSError{Err: "no such host", Name: "other.example.test", IsNotFound: true}
	}
	if _, err := fresh.DialContext(t.Context(), "tcp", net.JoinHostPort("other.example.test", port)); err == nil || calls != 1 {
		t.Fatalf("not-found host: calls=%d err=%v", calls, err)
	}
}

func TestRememberedControlAddressesExpireAfterADay(t *testing.T) {
	if controlAddressStale(time.Now().Add(-23 * time.Hour).Unix()) {
		t.Fatal("an address remembered 23 hours ago is stale")
	}
	if !controlAddressStale(time.Now().Add(-25 * time.Hour).Unix()) {
		t.Fatal("an address remembered 25 hours ago is still used")
	}
}
