package privatepreviewproxy

import (
	"errors"
	"net"
	"net/http"
	"os"
	"runtime"
	"testing"
)

func startPeerTestProxy(t *testing.T, owner func(net.Conn) (uint32, error), ownerUID uint32) *AccessProxy {
	t.Helper()
	proxy, err := StartAccessProxy(t.Context(), AccessProxyConfig{Source: &accessTestSource{routes: []AccessRoute{{MatchType: AccessMatchExact, Hostname: "one.private.example.test"}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Close() })
	proxy.peerOwner, proxy.ownerUID = owner, ownerUID
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
	if proxy.peerOwner == nil || proxy.ownerUID != uint32(os.Geteuid()) {
		t.Fatalf("proxy does not enforce the owning user by default (owner %v, uid %d)", proxy.peerOwner != nil, proxy.ownerUID)
	}
	if code := fetchPAC(t, proxy); code != http.StatusOK {
		t.Fatalf("own connection through the real lookup: status %d refusals %v", code, proxy.Refusals())
	}
}
