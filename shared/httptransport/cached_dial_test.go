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
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/dnscap"
)

// fakeNet maps public test addresses and names to real listeners; a name answers as DNS plus connect would.
type fakeNet struct {
	mu    sync.Mutex
	to    map[string]string // address dialed -> loopback listener
	roots *x509.CertPool
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
	f = &fakeNet{roots: roots, to: map[string]string{"203.0.113.1": server.Listener.Addr().String()}, names: map[string]string{"example.com": "203.0.113.1"}}
	get = f.getter(t, "example.com")
	dnscap.Verified("example.com", &net.TCPAddr{IP: net.IPv4(203, 0, 113, 1), Port: 443}) // the cache is the process's
	if proto, err := get(); err != nil || proto != "HTTP/2.0" {
		t.Fatalf("first request: %q %v", proto, err)
	}
	f.took()
	return f, get
}

func (f *fakeNet) getter(t *testing.T, host string) func() (string, error) {
	config := DevelopmentConfig()
	config.ProxySource, config.DialContext, config.CacheDNS = noProxy{}, f.dial, true
	config.TLSConfig = &tls.Config{RootCAs: f.roots, MinVersion: tls.VersionTLS12}
	return func() (string, error) {
		transport, err := New(config) // a fresh connection pool: every get dials
		if err != nil {
			t.Fatal(err)
		}
		defer transport.CloseIdleConnections()
		response, err := (&http.Client{Transport: transport, Timeout: 30 * time.Second}).Get("https://" + host + "/")
		if err != nil {
			return "", err
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		return string(body), err
	}
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

// aged makes the kept address 11 minutes old (the cache only loads a newer entry, so a name of its own per test).
func aged(t *testing.T, host, address string, age time.Duration) {
	t.Helper()
	dnscap.Persist([]byte(`{"`+host+`":{"address":"`+address+`","verified":"`+time.Now().Add(-age).UTC().Format(time.RFC3339)+`"}}`), nil)
}

func TestAnElevenMinuteOldAddressIsDialedWithoutWaitingForDeadDNS(t *testing.T) {
	f, _ := cachedFixture(t)
	get := f.getter(t, "old.example.com")
	aged(t, "old.example.com", "203.0.113.1", 11*time.Minute)
	f.slow = 20 * time.Second
	started := time.Now()
	if _, err := get(); err != nil || time.Since(started) > 2*time.Second {
		t.Fatalf("%v after %s", err, time.Since(started))
	}
	if got := f.took(); got[0] != "203.0.113.1" {
		t.Fatalf("dialed %v", got)
	}
}

// The old server still answers TLS after the name moved: the next dial uses it, and the refresh it starts moves on.
func TestRefreshFindsAMovedHostWhileTheOldAddressAnswers(t *testing.T) {
	f, _ := cachedFixture(t)
	get := f.getter(t, "moved.example.com")
	aged(t, "moved.example.com", "203.0.113.1", 11*time.Minute)
	f.to["203.0.113.2"], f.names["moved.example.com"] = f.to["203.0.113.1"], "203.0.113.2"
	if _, err := get(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); dnscap.Cached("moved.example.com").String() != "203.0.113.2"; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("kept %s", dnscap.Cached("moved.example.com"))
		}
	}
	f.took()
	if _, err := get(); err != nil {
		t.Fatal(err)
	}
	dialed(t, f, "203.0.113.2")
}

// Another host answering at the kept or the looked-up address never gets a request and is never kept.
func TestPoisonedAddressIsRefusedByTLS(t *testing.T) {
	f, get := cachedFixture(t)
	var requests atomic.Int32
	impostor := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	impostor.TLS = &tls.Config{Certificates: []tls.Certificate{impostorCertificate(t)}}
	impostor.StartTLS()
	defer impostor.Close()
	bad := impostor.Listener.Addr().String()
	f.to["203.0.113.66"] = bad
	dnscap.Verified("example.com", &net.TCPAddr{IP: net.IPv4(203, 0, 113, 66), Port: 443})
	if _, err := get(); err != nil { // the kept address is refused, the looked-up one proves itself
		t.Fatal(err)
	}
	dialed(t, f, "203.0.113.66", "example.com")
	if kept := dnscap.Cached("example.com").String(); kept != "203.0.113.1" {
		t.Fatalf("kept %s", kept)
	}
	poisoned := f.getter(t, "poisoned.example.com") // the background lookup is poisoned: not kept
	aged(t, "poisoned.example.com", "203.0.113.1", 11*time.Minute)
	f.names["poisoned.example.com"] = "203.0.113.66"
	if _, err := poisoned(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if got := f.took(); len(got) != 2 || got[1] != "poisoned.example.com" {
		t.Fatalf("no background lookup: %v", got)
	}
	if kept := dnscap.Cached("poisoned.example.com").String(); kept != "203.0.113.1" {
		t.Fatalf("kept %s", kept)
	}
	if requests.Load() != 0 {
		t.Fatalf("the impostor got %d requests", requests.Load())
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
