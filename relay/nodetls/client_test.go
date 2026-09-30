package nodetls

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"github.com/quic-go/quic-go"
	"math/big"
	"net"
	"testing"
	"time"
)

func certificate(t *testing.T, expired bool, usage x509.ExtKeyUsage) (tls.Certificate, string) {
	t.Helper()
	pub, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	until := now.Add(time.Hour)
	if expired {
		until = now.Add(-time.Minute)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: now.Add(-time.Hour), NotAfter: until, ExtKeyUsage: []x509.ExtKeyUsage{usage}, KeyUsage: x509.KeyUsageDigitalSignature, DNSNames: []string{"hosted.example.test"}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, private)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: private, Leaf: leaf}, base64.RawURLEncoding.EncodeToString(digest[:])
}

func TestPinnedTLSWire(t *testing.T) {
	for _, transport := range []string{"tcp", "quic"} {
		for _, name := range []string{"correct", "mismatch", "expired", "client-only"} {
			t.Run(transport+"/"+name, func(t *testing.T) {
				usage := x509.ExtKeyUsageServerAuth
				if name == "client-only" {
					usage = x509.ExtKeyUsageClientAuth
				}
				cert, pin := certificate(t, name == "expired", usage)
				if name == "mismatch" {
					_, pin = certificate(t, false, usage)
				}
				base := &tls.Config{MinVersion: tls.VersionTLS13, NextProtos: []string{"pin-test"}, ServerName: "127.0.0.1"}
				client, err := ClientConfig(base, pin)
				if err != nil {
					t.Fatal(err)
				}
				server := &tls.Config{MinVersion: tls.VersionTLS13, NextProtos: []string{"pin-test"}, Certificates: []tls.Certificate{cert}}
				ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
				defer cancel()
				if transport == "tcp" {
					listener, e := net.Listen("tcp", "127.0.0.1:0")
					if e != nil {
						t.Fatal(e)
					}
					defer listener.Close()
					done := make(chan struct{})
					go func() {
						defer close(done)
						c, e := listener.Accept()
						if e != nil {
							return
						}
						defer c.Close()
						_ = tls.Server(c, server).HandshakeContext(ctx)
					}()
					raw, e := (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
					if e != nil {
						t.Fatal(e)
					}
					c := tls.Client(raw, client)
					err = c.HandshakeContext(ctx)
					_ = c.Close()
					<-done
				} else {
					listener, e := quic.ListenAddr("127.0.0.1:0", server, nil)
					if e != nil {
						t.Fatal(e)
					}
					defer listener.Close()
					done := make(chan struct{})
					go func() {
						defer close(done)
						c, e := listener.Accept(ctx)
						if e == nil {
							<-ctx.Done()
							_ = c.CloseWithError(0, "")
						}
					}()
					c, e := quic.DialAddr(ctx, listener.Addr().String(), client, nil)
					err = e
					if c != nil {
						_ = c.CloseWithError(0, "")
					}
					cancel()
					<-done
				}
				if (err == nil) != (name == "correct") {
					t.Fatalf("handshake error = %v", err)
				}
				if base.InsecureSkipVerify || base.VerifyConnection != nil {
					t.Fatal("mutated shared TLS config")
				}
			})
		}
	}
}

func TestHostedTLSRetainsPKI(t *testing.T) {
	cert, _ := certificate(t, false, x509.ExtKeyUsageServerAuth)
	roots := x509.NewCertPool()
	roots.AddCert(cert.Leaf)
	base := &tls.Config{RootCAs: roots, ServerName: "hosted.example.test"}
	config, err := ClientConfig(base, "")
	if err != nil || config.InsecureSkipVerify || config.RootCAs != roots || config.ServerName != base.ServerName {
		t.Fatalf("hosted PKI changed: %v", err)
	}
	for _, pin := range []string{"bad", base64.RawURLEncoding.EncodeToString(make([]byte, 31)), base64.URLEncoding.EncodeToString(make([]byte, 32))} {
		if _, err := ClientConfig(base, pin); err == nil {
			t.Fatal("accepted malformed pin")
		}
	}
}

func TestHostedTLSWire(t *testing.T) {
	cert, _ := certificate(t, false, x509.ExtKeyUsageServerAuth)
	for _, name := range []string{"trusted", "wrong-host", "untrusted"} {
		t.Run(name, func(t *testing.T) {
			roots := x509.NewCertPool()
			if name != "untrusted" {
				roots.AddCert(cert.Leaf)
			}
			hostname := "hosted.example.test"
			if name == "wrong-host" {
				hostname = "wrong.example.test"
			}
			client, err := ClientConfig(&tls.Config{RootCAs: roots, ServerName: hostname, MinVersion: tls.VersionTLS13}, "")
			if err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			done := make(chan struct{})
			go func() {
				defer close(done)
				raw, err := listener.Accept()
				if err != nil {
					return
				}
				defer raw.Close()
				_ = tls.Server(raw, &tls.Config{Certificates: []tls.Certificate{cert}}).HandshakeContext(ctx)
			}()
			raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			c := tls.Client(raw, client)
			err = c.HandshakeContext(ctx)
			_ = c.Close()
			<-done
			if (err == nil) != (name == "trusted") {
				t.Fatalf("hosted handshake: %v", err)
			}
		})
	}
}
