package httptransport

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/dnscap"
)

// fakeNet maps public test addresses and names to real listeners; a name answers as DNS plus connect would.
type fakeNet struct {
	mu    sync.Mutex
	to    map[string]string // address dialed -> loopback listener
	names map[string]string // host -> public address; absent: the lookup fails
	slow  time.Duration     // how long a lookup takes
	dials []string
}

type publicConn struct {
	net.Conn
	remote net.Addr
}

func (c publicConn) RemoteAddr() net.Addr { return c.remote }

func (f *fakeNet) dial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, _ := net.SplitHostPort(address)
	f.mu.Lock()
	f.dials = append(f.dials, host)
	ip, slow := host, time.Duration(0)
	if net.ParseIP(host) == nil {
		ip, slow = f.names[host], f.slow
	}
	target := f.to[ip]
	f.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(slow):
	}
	if ip == "" {
		return nil, &net.DNSError{Err: "i/o timeout", Name: host, IsTimeout: true}
	}
	if target == "" {
		return nil, &net.OpError{Op: "dial", Net: network, Err: errors.New("connection refused")}
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, network, target)
	if err != nil {
		return nil, err
	}
	return publicConn{conn, net.TCPAddrFromAddrPort(netip.MustParseAddrPort(net.JoinHostPort(ip, port)))}, nil
}

func (f *fakeNet) took() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.dials
	f.dials = nil
	return out
}

func cachedFixture(t *testing.T) (f *fakeNet, get func() (string, error)) {
	t.Helper()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, r.Proto) }))
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	f = &fakeNet{to: map[string]string{"203.0.113.1": server.Listener.Addr().String()}, names: map[string]string{"example.com": "203.0.113.1"}}
	config := DevelopmentConfig()
	config.ProxySource, config.DialContext, config.CacheDNS = noProxy{}, f.dial, true
	config.TLSConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	get = func() (string, error) {
		transport, err := New(config) // a fresh connection pool: every get dials
		if err != nil {
			t.Fatal(err)
		}
		defer transport.CloseIdleConnections()
		response, err := (&http.Client{Transport: transport, Timeout: 30 * time.Second}).Get("https://example.com/")
		if err != nil {
			return "", err
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		return string(body), err
	}
	dnscap.Verified("example.com", &net.TCPAddr{IP: net.IPv4(203, 0, 113, 1), Port: 443}) // the cache is the process's
	if proto, err := get(); err != nil || proto != "HTTP/2.0" {
		t.Fatalf("first request: %q %v", proto, err)
	}
	f.took()
	return f, get
}

func dialed(t *testing.T, f *fakeNet, want ...string) {
	t.Helper()
	if got := f.took(); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("dialed %v, want %v", got, want)
	}
}

func TestSecondDialDoesNotLookUpBehindADeadNameserver(t *testing.T) {
	f, get := cachedFixture(t)
	f.slow = 20 * time.Second
	started := time.Now()
	if proto, err := get(); err != nil || proto != "HTTP/2.0" || time.Since(started) > 2*time.Second {
		t.Fatalf("%q %v after %s", proto, err, time.Since(started))
	}
	dialed(t, f, "203.0.113.1")
}

func TestChangedAddressIsFoundOnTheFirstFailedDial(t *testing.T) {
	f, get := cachedFixture(t)
	f.to["203.0.113.2"] = f.to["203.0.113.1"]
	f.to["203.0.113.1"], f.names["example.com"] = "", "203.0.113.2"
	if _, err := get(); err != nil {
		t.Fatal(err)
	}
	dialed(t, f, "203.0.113.1", "example.com")
	if _, err := get(); err != nil {
		t.Fatal(err)
	}
	dialed(t, f, "203.0.113.2")
}

func TestLastGoodIsUsedWhenEveryNameserverIsDead(t *testing.T) {
	f, get := cachedFixture(t)
	f.slow = 20 * time.Second
	dnscap.Expire("example.com", netip.MustParseAddr("203.0.113.1"))
	started := time.Now()
	if _, err := get(); err != nil || time.Since(started) > dnscap.LastGoodWait+2*time.Second {
		t.Fatalf("%v after %s", err, time.Since(started))
	}
	dialed(t, f, "example.com", "203.0.113.1")
	if _, fresh := dnscap.Cached("example.com"); !fresh {
		t.Fatal("the last good address that worked is not fresh again")
	}
}

// Another host answering at the kept or the looked-up address never gets a request and is never kept.
func TestPoisonedAddressIsRefusedByTLS(t *testing.T) {
	f, get := cachedFixture(t)
	requests := 0
	impostor := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	impostor.TLS = &tls.Config{Certificates: []tls.Certificate{impostorCertificate(t)}}
	impostor.StartTLS()
	defer impostor.Close()
	good := f.to["203.0.113.1"]
	f.to["203.0.113.1"] = impostor.Listener.Addr().String()
	if _, err := get(); err == nil {
		t.Fatal("a kept address with another certificate was accepted")
	}
	if _, fresh := dnscap.Cached("example.com"); fresh {
		t.Fatal("a kept address that failed TLS is still fresh")
	}
	f.to["203.0.113.2"], f.to["203.0.113.1"], f.names["example.com"] = f.to["203.0.113.1"], good, "203.0.113.2"
	if _, err := get(); err != nil { // the lookup is poisoned, the last good address still proves itself
		t.Fatal(err)
	}
	if a, _ := dnscap.Cached("example.com"); a.String() != "203.0.113.1" {
		t.Fatalf("kept %s", a)
	}
	if requests != 0 {
		t.Fatalf("the impostor got %d requests", requests)
	}
}

func TestConcurrentDialsDoNotWaitForEachOther(t *testing.T) {
	f, get := cachedFixture(t)
	f.slow = 20 * time.Second
	started := time.Now()
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if _, err := get(); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if time.Since(started) > 5*time.Second {
		t.Fatalf("took %s", time.Since(started))
	}
}

func impostorCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"example.com"}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
