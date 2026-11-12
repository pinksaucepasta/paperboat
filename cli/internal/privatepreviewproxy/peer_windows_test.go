//go:build windows

package privatepreviewproxy

import (
	"context"
	"errors"
	"io"
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

// X-19 W: a client on either loopback family that sends and half-closes at once (what a script does) is served
// through the real kernel lookup, as the CLI's `pb access machine --listen [::1]:Q` runs it.
func TestWindowsProxyServesAHalfClosingClientOnEveryLoopback(t *testing.T) {
	origin, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer origin.Close()
	go func() { // reads to EOF, then answers: needs the proxy to half-close toward it
		for {
			c, err := origin.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); b, _ := io.ReadAll(c); _, _ = c.Write(b) }()
		}
	}()
	for _, listen := range []string{"127.0.0.1:0", "[::1]:0"} {
		proxy, err := Start(t.Context(), Config{ListenAddress: listen, Dial: func(ctx context.Context) (io.ReadWriteCloser, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp4", origin.Addr().String())
		}})
		if err != nil {
			t.Fatalf("%s: %v", listen, err)
		}
		conn, err := net.Dial("tcp", proxy.URL[len("http://"):])
		if err != nil {
			t.Fatalf("%s: %v", listen, err)
		}
		if _, err = conn.Write([]byte("hello")); err != nil {
			t.Fatalf("%s: %v", listen, err)
		}
		_ = conn.(*net.TCPConn).CloseWrite()
		got, err := io.ReadAll(conn)
		if err != nil || string(got) != "hello" {
			t.Fatalf("%s: echo %q err %v refusals %v", listen, got, err, proxy.Refusals())
		}
		_ = conn.Close()
		_ = proxy.Close()
	}
}
