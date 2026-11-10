//go:build windows

package privatepreviewproxy

import (
	"errors"
	"net"
	"net/http"
	"testing"
)

func startSIDPeerProxy(t *testing.T, owner func(net.Conn) (string, error), sid string) *AccessProxy {
	t.Helper()
	proxy, err := StartAccessProxy(t.Context(), AccessProxyConfig{gate: &peerGate{ownerSID: owner, sid: sid}, Source: &accessTestSource{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = proxy.Close() })
	return proxy
}

// F23-01 W: the proxy serves the user that runs it and refuses another local user, or a connection it cannot attribute.
func TestWindowsProxyAdmitsOnlyTheOwningUser(t *testing.T) {
	const owner, other = "S-1-5-21-1-2-3-1001", "S-1-5-21-1-2-3-1002"
	for name, c := range map[string]struct {
		sid    string
		err    error
		status int
		reason string
	}{
		"owner":        {owner, nil, http.StatusOK, ""},
		"other user":   {other, nil, http.StatusForbidden, refusalOtherUser},
		"unattributed": {"", errors.New("no owner"), http.StatusForbidden, refusalLookupFailed},
	} {
		proxy := startSIDPeerProxy(t, func(net.Conn) (string, error) { return c.sid, c.err }, owner)
		if code := fetchPAC(t, proxy); code != c.status {
			t.Fatalf("%s: status %d, want %d", name, code, c.status)
		}
		if got := proxy.Refusals()[c.reason]; c.reason != "" && got != 1 || c.reason == "" && len(proxy.Refusals()) != 0 {
			t.Fatalf("%s: refusals %v, want one %q", name, proxy.Refusals(), c.reason)
		}
	}
}

// The default gate uses the kernel lookup and this process's own SID: its own loopback connection is served.
func TestWindowsProxyAttributesItsOwnProcessWithTheKernelLookup(t *testing.T) {
	proxy, err := StartAccessProxy(t.Context(), AccessProxyConfig{Source: &accessTestSource{}})
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	if proxy.gate.ownerSID == nil || proxy.gate.sid == "" {
		t.Fatal("proxy does not enforce the owning user by default")
	}
	if code := fetchPAC(t, proxy); code != http.StatusOK {
		t.Fatalf("own connection through the real lookup: status %d refusals %v", code, proxy.Refusals())
	}
}
