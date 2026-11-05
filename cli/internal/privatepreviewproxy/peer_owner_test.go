package privatepreviewproxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func startPeerTestProxy(t *testing.T, owner func(net.Conn) (uint32, error), ownerUID uint32) *AccessProxy {
	t.Helper()
	proxy, err := StartAccessProxy(t.Context(), AccessProxyConfig{gate: &peerGate{owner: owner, uid: ownerUID}, Source: &accessTestSource{routes: []AccessRoute{{MatchType: AccessMatchExact, Hostname: "one.private.example.test"}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Close() })
	return proxy
}

func fetchPAC(t *testing.T, proxy *AccessProxy) int {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	response, err := client.Get(proxy.PACURL)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	return response.StatusCode
}

func TestProxyRefusesAConnectionFromAnotherUserAndCountsIt(t *testing.T) {
	proxy := startPeerTestProxy(t, func(net.Conn) (uint32, error) { return 502, nil }, 501)
	if code := fetchPAC(t, proxy); code != http.StatusForbidden {
		t.Fatalf("another user's connection: status %d, want 403", code)
	}
	if got := proxy.Refusals()[refusalOtherUser]; got != 1 {
		t.Fatalf("other_user refusals = %d, want 1 (%v)", got, proxy.Refusals())
	}
}

func TestProxyRefusesAConnectionItCannotAttribute(t *testing.T) {
	proxy := startPeerTestProxy(t, func(net.Conn) (uint32, error) { return 0, errors.New("no owner") }, 501)
	if code := fetchPAC(t, proxy); code != http.StatusForbidden {
		t.Fatalf("unattributed connection: status %d, want 403 (fail closed)", code)
	}
	if got := proxy.Refusals()[refusalLookupFailed]; got != 1 {
		t.Fatalf("lookup_failed refusals = %v", proxy.Refusals())
	}
}

func TestProxyServesTheOwningUser(t *testing.T) {
	proxy := startPeerTestProxy(t, func(net.Conn) (uint32, error) { return 501, nil }, 501)
	if code := fetchPAC(t, proxy); code != http.StatusOK {
		t.Fatalf("owner's connection: status %d, want 200", code)
	}
	if len(proxy.Refusals()) != 0 {
		t.Fatalf("owner counted as refused: %v", proxy.Refusals())
	}
}

// The kernel lookup itself: this process's own loopback connection is attributed to this process's uid.
func TestProxyAttributesItsOwnProcessWithTheKernelLookup(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("no peer attribution on this platform")
	}
	proxy, err := StartAccessProxy(t.Context(), AccessProxyConfig{Source: &accessTestSource{}})
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	if proxy.gate.owner == nil || proxy.gate.uid != uint32(os.Geteuid()) {
		t.Fatalf("proxy does not enforce the owning user by default (owner %v, uid %d)", proxy.gate.owner != nil, proxy.gate.uid)
	}
	if code := fetchPAC(t, proxy); code != http.StatusOK {
		t.Fatalf("own connection through the real lookup: status %d refusals %v", code, proxy.Refusals())
	}
}

func startTCPPeerProxy(t *testing.T, owner func(net.Conn) (uint32, error), uid uint32, dials *atomic.Int32) *Proxy {
	t.Helper()
	proxy, err := Start(t.Context(), Config{gate: &peerGate{owner: owner, uid: uid}, Dial: func(context.Context) (io.ReadWriteCloser, error) {
		dials.Add(1)
		left, right := net.Pipe()
		go func() { defer right.Close(); _, _ = io.Copy(right, right) }()
		return left, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Close() })
	return proxy
}

func echoes(t *testing.T, proxy *Proxy) bool {
	t.Helper()
	conn, err := net.Dial("tcp4", strings.TrimPrefix(proxy.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write([]byte("x")); err != nil {
		return false
	}
	buf := make([]byte, 1)
	_, err = io.ReadFull(conn, buf)
	return err == nil
}

// The private-TCP listener refuses another user before opening any stream for it (a dial is a grant and a carrier stream).
func TestPrivateTCPProxyRefusesAnotherUserBeforeDialing(t *testing.T) {
	var dials atomic.Int32
	proxy := startTCPPeerProxy(t, func(net.Conn) (uint32, error) { return 502, nil }, 501, &dials)
	preflight := dials.Load()
	if echoes(t, proxy) {
		t.Fatal("another user's connection was carried")
	}
	if dials.Load() != preflight {
		t.Fatalf("a stream was opened for another user (%d dials after preflight)", dials.Load()-preflight)
	}
	if got := proxy.Refusals()[refusalOtherUser]; got != 1 {
		t.Fatalf("other_user refusals = %v", proxy.Refusals())
	}
}

func TestPrivateTCPProxyCarriesTheOwningUser(t *testing.T) {
	var dials atomic.Int32
	proxy := startTCPPeerProxy(t, func(net.Conn) (uint32, error) { return 501, nil }, 501, &dials)
	if !echoes(t, proxy) {
		t.Fatal("the owner's connection was refused")
	}
}

func TestPrivateTCPProxyRefusesAnIPv6ListenerItCannotRestrict(t *testing.T) {
	if platformPeerOwner == nil {
		t.Skip("no peer attribution on this platform")
	}
	_, err := Start(t.Context(), Config{ListenAddress: "[::1]:0", Dial: func(context.Context) (io.ReadWriteCloser, error) {
		left, right := net.Pipe()
		go right.Close()
		return left, nil
	}})
	if !errors.Is(err, ErrLoopbackUnrestricted) {
		t.Fatalf("IPv6 loopback listener: %v, want ErrLoopbackUnrestricted", err)
	}
}
