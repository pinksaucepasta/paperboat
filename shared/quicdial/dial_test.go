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
	"strconv"
	"testing"
	"time"

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

// A name with both an A and an AAAA record must reach a server that only listens on IPv6: quic.DialAddr binds an
// IPv4 socket and cannot (the v6-only NAT64 host case). Skipped where localhost lacks ::1 or 127.0.0.1.
func TestDialNameReachesIPv6OnlyServer(t *testing.T) {
	certificate := serverCertificate(t)
	port := listen(t, "udp6", net.IPv6loopback, certificate)
	if ips, err := net.DefaultResolver.LookupIPAddr(context.Background(), "localhost"); err != nil || len(ips) == 0 {
		t.Skip("localhost does not resolve")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connection, err := Dial(ctx, net.JoinHostPort("localhost", strconv.Itoa(port)), trusting(certificate), nil, false)
	if err != nil {
		t.Fatalf("dial localhost over IPv6: %v", err)
	}
	if got := connection.RemoteAddr().(*net.UDPAddr).IP; !got.Equal(net.IPv6loopback) {
		t.Fatalf("remote %v, want ::1", got)
	}
	if connection.ConnectionState().TLS.ServerName != "localhost" {
		t.Fatalf("server name %q, want the host name", connection.ConnectionState().TLS.ServerName)
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
