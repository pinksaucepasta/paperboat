package quicdial

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/dnscap"
	"github.com/quic-go/quic-go"
)

func serverCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "quicdial test"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"localhost"}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// listen starts a QUIC echo-less server on one loopback family; the test skips when the host has no such loopback.
func listen(t *testing.T, network string, ip net.IP, certificate tls.Certificate) int {
	t.Helper()
	socket, err := net.ListenUDP(network, &net.UDPAddr{IP: ip})
	if err != nil {
		t.Skipf("no %s loopback: %v", network, err)
	}
	listener, err := quic.Listen(socket, &tls.Config{Certificates: []tls.Certificate{certificate}, NextProtos: []string{"t"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close(); _ = socket.Close() })
	go func() {
		for {
			connection, err := listener.Accept(context.Background())
			if err != nil {
				return
			}
			_ = connection
		}
	}()
	return socket.LocalAddr().(*net.UDPAddr).Port
}

func trusting(certificate tls.Certificate) *tls.Config {
	leaf, _ := x509.ParseCertificate(certificate.Certificate[0])
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	return &tls.Config{RootCAs: roots, NextProtos: []string{"t"}, MinVersion: tls.VersionTLS13}
}

// quic.DialAddr binds an IPv4 socket, so it can never reach an IPv6 address; the dial must use a socket of the
// server's family. The name resolves (fake resolver) only to ::1, as a DNS64 resolver answers a v6-only host.
func TestDialNameReachesIPv6OnlyServer(t *testing.T) {
	certificate := serverCertificate(t)
	port := listen(t, "udp6", net.IPv6loopback, certificate)
	defer func(previous func(context.Context, string) ([]net.IPAddr, error)) { lookup = previous }(lookup)
	lookup = func(_ context.Context, host string) ([]net.IPAddr, error) {
		if host != "edge.example.test" {
			t.Fatalf("resolved %q", host)
		}
		return []net.IPAddr{{IP: net.IPv6loopback}}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conf := trusting(certificate)
	conf.ServerName = "localhost" // the test certificate names localhost
	connection, err := Dial(ctx, net.JoinHostPort("edge.example.test", strconv.Itoa(port)), conf, nil, false)
	if err != nil {
		t.Fatalf("dial over IPv6: %v", err)
	}
	if got := connection.RemoteAddr().(*net.UDPAddr).IP; !got.Equal(net.IPv6loopback) {
		t.Fatalf("remote %v, want ::1", got)
	}
	_ = connection.CloseWithError(0, "")
}

// Without a configured ServerName the host name is used, not the resolved address.
func TestDialServerNameDefaultsToTheHost(t *testing.T) {
	certificate := serverCertificate(t)
	port := listen(t, "udp4", net.IPv4(127, 0, 0, 1), certificate)
	defer func(previous func(context.Context, string) ([]net.IPAddr, error)) { lookup = previous }(lookup)
	lookup = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.IPv4(127, 0, 0, 1)}}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conf := trusting(certificate)
	connection, err := Dial(ctx, net.JoinHostPort("localhost", strconv.Itoa(port)), conf, nil, false)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if got := connection.ConnectionState().TLS.ServerName; got != "localhost" {
		t.Fatalf("server name %q, want localhost", got)
	}
	_ = connection.CloseWithError(0, "")
}

func TestDialIPv4LiteralStaysIPv4(t *testing.T) {
	certificate := serverCertificate(t)
	port := listen(t, "udp4", net.IPv4(127, 0, 0, 1), certificate)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conf := trusting(certificate)
	conf.ServerName = "localhost"
	connection, err := Dial(ctx, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), conf, nil, true)
	if err != nil {
		t.Fatalf("dial IPv4 literal: %v", err)
	}
	_ = connection.CloseWithError(0, "")
}

func TestDialRejectsBadAddress(t *testing.T) {
	for _, address := range []string{"no-port", "host:0", "host:70000", "host:x"} {
		if _, err := Dial(context.Background(), address, &tls.Config{}, nil, false); err == nil {
			t.Errorf("Dial(%q) succeeded", address)
		}
	}
}

// A kept address nobody answers at: after dnscap.KeptDialWait the name is looked up and dialed too, in one Dial.
func TestDialLeavesADeadKeptAddress(t *testing.T) {
	certificate := serverCertificate(t)
	port := listen(t, "udp4", net.IPv4(127, 0, 0, 1), certificate)
	defer func(previous func(context.Context, string) ([]net.IPAddr, error)) { lookup = previous }(lookup)
	lookups := 0
	lookup = func(context.Context, string) ([]net.IPAddr, error) {
		lookups++
		return []net.IPAddr{{IP: net.IPv4(127, 0, 0, 1)}}, nil
	}
	defer func() { cached = dnscap.Cached }()
	cached = func(string) netip.Addr { return netip.MustParseAddr("192.0.2.1") } // nobody answers there
	conf := trusting(certificate)
	conf.ServerName = "localhost"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started := time.Now()
	connection, err := Dial(ctx, net.JoinHostPort("moved.example.test", strconv.Itoa(port)), conf, &quic.Config{HandshakeIdleTimeout: 8 * time.Second}, false)
	if err != nil || lookups != 1 || time.Since(started) < dnscap.KeptDialWait {
		t.Fatalf("lookups %d after %s: %v", lookups, time.Since(started), err)
	}
	_ = connection.CloseWithError(0, "")
}

// A kept address that answers is dialed without a lookup.
func TestDialUsesAKeptAddressWithoutALookup(t *testing.T) {
	certificate := serverCertificate(t)
	port := listen(t, "udp4", net.IPv4(127, 0, 0, 1), certificate)
	defer func(previous func(context.Context, string) ([]net.IPAddr, error)) { lookup = previous }(lookup)
	lookup = func(context.Context, string) ([]net.IPAddr, error) {
		t.Error("looked up although an address is kept")
		return nil, &net.DNSError{Err: "dead", IsTimeout: true}
	}
	defer func() { cached = dnscap.Cached }()
	cached = func(string) netip.Addr { return netip.MustParseAddr("127.0.0.1") }
	conf := trusting(certificate)
	conf.ServerName = "localhost"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connection, err := Dial(ctx, net.JoinHostPort("kept.example.test", strconv.Itoa(port)), conf, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	_ = connection.CloseWithError(0, "")
}

// A relay or edge is verified by its pin, not a WebPKI chain; a handshake that checked nothing, is unfinished, names
// another host, or resumed past VerifyPeerCertificate never counts.
func TestVerifiedAcceptsOnlyAFinishedCheckForTheHost(t *testing.T) {
	host := "relay.example.test"
	done := tls.ConnectionState{HandshakeComplete: true}
	connection := &tls.Config{ServerName: host, InsecureSkipVerify: true, VerifyConnection: func(tls.ConnectionState) error { return nil }}
	peer := &tls.Config{ServerName: host, InsecureSkipVerify: true, VerifyPeerCertificate: func([][]byte, [][]*x509.Certificate) error { return nil }}
	chain := tls.ConnectionState{HandshakeComplete: true, VerifiedChains: [][]*x509.Certificate{{}}}
	for _, c := range []struct {
		name   string
		config *tls.Config
		state  tls.ConnectionState
		want   bool
	}{
		{"pinned by VerifyConnection", connection, done, true},
		{"pinned by VerifyPeerCertificate", peer, done, true},
		{"resumed past VerifyPeerCertificate", peer, tls.ConnectionState{HandshakeComplete: true, DidResume: true}, false},
		{"unfinished", connection, tls.ConnectionState{}, false},
		{"pin for another name", &tls.Config{ServerName: "other.example.test", InsecureSkipVerify: true, VerifyConnection: connection.VerifyConnection}, done, false},
		{"nothing checked", &tls.Config{ServerName: host, InsecureSkipVerify: true}, done, false},
		{"WebPKI chain", &tls.Config{ServerName: host}, chain, true},
		{"chain for another name", &tls.Config{ServerName: "other.example.test"}, chain, false},
	} {
		if got := verified(c.config, host, c.state); got != c.want {
			t.Errorf("%s: verified = %v, want %v", c.name, got, c.want)
		}
	}
}

// A kept address that never answers, under a 2 s budget (the HTTP/3 attempt's), still connects through the lookup.
func TestShortBudgetReachesTheLookup(t *testing.T) {
	certificate := serverCertificate(t)
	port := listen(t, "udp4", net.IPv4(127, 0, 0, 1), certificate)
	defer func(previous func(context.Context, string) ([]net.IPAddr, error)) { lookup = previous }(lookup)
	lookup = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.IPv4(127, 0, 0, 1)}}, nil
	}
	defer func() { cached = dnscap.Cached }()
	cached = func(string) netip.Addr { return netip.MustParseAddr("192.0.2.1") }
	conf := trusting(certificate)
	conf.ServerName = "localhost"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	connection, err := Dial(ctx, net.JoinHostPort("blackholed.example.test", strconv.Itoa(port)), conf, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	_ = connection.CloseWithError(0, "")
}
