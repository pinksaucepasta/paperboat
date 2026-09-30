package main

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/config"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInstallationChallengeBindsNodeAndNonce(t *testing.T) {
	const secret = "runtime-credential-for-test-0123456789"
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
	handler := installationChallenge("node_a", "edge.example.test", secret, next)
	nonce := base64.RawURLEncoding.EncodeToString([]byte(strings.Repeat("n", 32)))
	request := httptest.NewRequest("GET", "https://edge.example.test/.well-known/paperboat-installation?challenge="+nonce, nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	var response map[string]string
	if recorder.Code != 200 || json.Unmarshal(recorder.Body.Bytes(), &response) != nil {
		t.Fatal("challenge failed")
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("paperboat-installation/v1\nnode_a\n" + nonce))
	if response["node_id"] != "node_a" || response["proof"] != base64.RawURLEncoding.EncodeToString(mac.Sum(nil)) {
		t.Fatal("proof not bound")
	}
	for _, target := range []string{"https://other.example.test/.well-known/paperboat-installation?challenge=" + nonce, "https://edge.example.test/.well-known/paperboat-installation?challenge=bad", "https://edge.example.test/.well-known/paperboat-installation?challenge=" + nonce + "&challenge=" + nonce} {
		r := httptest.NewRecorder()
		handler.ServeHTTP(r, httptest.NewRequest("GET", target, nil))
		if r.Code == 200 {
			t.Fatal("invalid challenge accepted")
		}
	}
}

func TestInfrastructureTLSNoSNIAndNamedRouteSelection(t *testing.T) {
	makeCertificate := func(name string, ip net.IP) (tls.Certificate, []byte, []byte) {
		t.Helper()
		public, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		template := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{name}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		if ip != nil {
			template.IPAddresses = []net.IP{ip}
		}
		der, err := x509.CreateCertificate(rand.Reader, template, template, public, private)
		if err != nil {
			t.Fatal(err)
		}
		keyDER, err := x509.MarshalPKCS8PrivateKey(private)
		if err != nil {
			t.Fatal(err)
		}
		certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
		keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
		cert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			t.Fatal(err)
		}
		cert.Leaf, err = x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		return cert, certPEM, keyPEM
	}
	infrastructure, certPEM, keyPEM := makeCertificate("infra.example.test", net.ParseIP("127.0.0.1"))
	browser, _, _ := makeCertificate("route.example.test", nil)
	directory := t.TempDir()
	certPath, keyPath := filepath.Join(directory, "cert.pem"), filepath.Join(directory, "key.pem")
	if err := os.WriteFile(certPath, certPEM, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		t.Fatal(err)
	}
	base := &tls.Config{MinVersion: tls.VersionTLS13, GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) { return &browser, nil }}
	for _, host := range []string{"127.0.0.1", "infra.example.test"} {
		server, err := infrastructureTLS(config.Deployment{ConnectorAdvertiseHost: host, InfrastructureTLSCertFile: certPath, InfrastructureTLSKeyFile: keyPath}, base)
		if err != nil {
			t.Fatal(err)
		}
		tests := []struct {
			name string
			cert tls.Certificate
		}{{"127.0.0.1", infrastructure}, {"route.example.test", browser}}
		if host == "infra.example.test" {
			tests = append(tests, struct {
				name string
				cert tls.Certificate
			}{host, infrastructure})
		}
		for _, test := range tests {
			t.Run(host+"/"+test.name, func(t *testing.T) {
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
					_ = tls.Server(raw, server).HandshakeContext(ctx)
				}()
				roots := x509.NewCertPool()
				roots.AddCert(test.cert.Leaf)
				raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
				if err != nil {
					t.Fatal(err)
				}
				connection := tls.Client(raw, &tls.Config{RootCAs: roots, ServerName: test.name, MinVersion: tls.VersionTLS13})
				err = connection.HandshakeContext(ctx)
				if err == nil && !connection.ConnectionState().PeerCertificates[0].Equal(test.cert.Leaf) {
					t.Error("selected wrong certificate")
				}
				_ = connection.Close()
				<-done
				if err != nil {
					t.Fatalf("TLS handshake: %v", err)
				}
			})
		}
	}
}
