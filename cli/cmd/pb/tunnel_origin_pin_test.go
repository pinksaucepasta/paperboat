package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/tunnelmanager"
)

func tunnelOriginFlagCommand(t *testing.T, args ...string) *cobra.Command {
	t.Helper()
	command := &cobra.Command{Use: "test"}
	tunnelRouteOriginFlags(command, true)
	if err := command.Flags().Parse(args); err != nil {
		t.Fatal(err)
	}
	return command
}

// A remote self-signed origin is pinned by fingerprint: --tls-pin takes the sha256: or openssl colon
// form; without --tls-pin the CLI shows the certificate and, with no terminal to confirm on, names the
// exact --tls-pin to pass. Leaving pinned drops the pin.
func TestTunnelRouteOriginPinFlags(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	leaf, err := x509.ParseCertificate(server.TLS.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	want := tunnelmanager.CertificateFingerprint(leaf)
	origin := api.TunnelRouteOrigin{Scheme: "https", Address: server.Listener.Addr().String(), TLS: &api.TunnelRouteTLS{Verification: "system"}}

	colons := strings.ToUpper(strings.TrimPrefix(want, "sha256:"))
	var pairs []string
	for i := 0; i < len(colons); i += 2 {
		pairs = append(pairs, colons[i:i+2])
	}
	got, err := applyTunnelRouteOriginFlags(tunnelOriginFlagCommand(t, "--tls-pin", strings.Join(pairs, ":")), origin)
	if err != nil || got.TLS.Verification != "pinned" || got.TLS.Pin == nil || *got.TLS.Pin != want {
		t.Fatalf("--tls-pin colon form = %+v, %v", got.TLS, err)
	}
	if _, err := applyTunnelRouteOriginFlags(tunnelOriginFlagCommand(t, "--tls-pin", want, "--tls-verification", "system"), origin); err == nil {
		t.Fatal("--tls-pin with system verification was accepted")
	}
	command := tunnelOriginFlagCommand(t, "--tls-verification", "pinned")
	command.SetErr(&strings.Builder{})
	if _, err := applyTunnelRouteOriginFlags(command, origin); err == nil || !strings.Contains(err.Error(), "--tls-pin "+want) {
		t.Fatalf("pinned without a terminal = %v, want the --tls-pin to pass", err)
	}
	pinned := got
	back, err := applyTunnelRouteOriginFlags(tunnelOriginFlagCommand(t, "--tls-verification", "system"), pinned)
	if err != nil || back.TLS.Verification != "system" || back.TLS.Pin != nil {
		t.Fatalf("back to system = %+v, %v", back.TLS, err)
	}
}

// pb tunnel doctor names what an origin's unverifiable certificate means and the setting that fixes it.
func TestOriginCertificateHintNamesTheFix(t *testing.T) {
	now := time.Now()
	certificate := func(subject, issuer string, names []string, notAfter time.Time) *x509.Certificate {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: subject}, Issuer: pkix.Name{CommonName: issuer}, DNSNames: names, NotBefore: now.Add(-time.Hour), NotAfter: notAfter}
		parent := template
		if issuer != subject {
			parent = &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: issuer}}
		}
		raw, err := x509.CreateCertificate(rand.Reader, template, parent, &key.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		leaf, err := x509.ParseCertificate(raw)
		if err != nil {
			t.Fatal(err)
		}
		return leaf
	}
	for _, test := range []struct {
		name string
		leaf *x509.Certificate
		want string
	}{
		{"caddy internal", certificate("", "Caddy Local Authority - ECC Intermediate", []string{"app.example.test"}, now.Add(time.Hour)), "insecure"},
		{"traefik default", certificate("TRAEFIK DEFAULT CERT", "TRAEFIK DEFAULT CERT", nil, now.Add(time.Hour)), "http entrypoint"},
		{"expired", certificate("nas", "Some CA", []string{"app.example.test"}, now.Add(-time.Minute)), "expired"},
		{"other name", certificate("nas", "Some CA", []string{"nas.lan"}, now.Add(time.Hour)), "--tls-server-name nas.lan"},
		{"self-signed", certificate("proxmox", "proxmox", []string{"app.example.test"}, now.Add(time.Hour)), "pinned"},
		{"private CA", certificate("app", "Corp CA", []string{"app.example.test"}, now.Add(time.Hour)), "--ca-reference"},
	} {
		if hint := originCertificateHint(test.leaf, "app.example.test", now); !strings.Contains(hint, test.want) {
			t.Errorf("%s: hint %q, want it to name %q", test.name, hint, test.want)
		}
	}
}
