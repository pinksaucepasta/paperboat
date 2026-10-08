package selfhost

import (
	"bytes"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCertificateRenewalPreservesPinAndLiveTLS(t *testing.T) {
	dir, secret, client, oldServer := testInstallation(t, false)
	oldServer.Close()
	s, err := load(dir)
	if err != nil {
		t.Fatal(err)
	}
	key, err := privateKey(s)
	if err != nil {
		t.Fatal(err)
	}
	old, err := certificatePEM(key, s.Config.EndpointHost, time.Now().AddDate(-1, 0, -1))
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(dir, "tls.crt"), old); err != nil {
		t.Fatal(err)
	}
	initial, err := TLSCertificate(dir)
	if err != nil {
		t.Fatal(err)
	}
	pinBefore, _ := CertificatePin(initial)
	server := httptest.NewUnstartedServer(Handler(dir, nil))
	server.TLS = liveTLSConfig(dir, initial)
	server.StartTLS()
	// httptest injects its own default certificate when GetCertificate is used.
	// Production ServeTLS uses the callback without a static certificate.
	server.TLS.Certificates = nil
	defer server.Close()
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/selfhost/inspect", bytes.NewReader([]byte(`{}`)))
	if resp, err := client.Do(req); err == nil {
		resp.Body.Close()
		t.Fatal("expired certificate accepted")
	}
	renewed, err := RenewCertificate(dir, time.Now())
	if err != nil || !renewed {
		t.Fatalf("renewal changed=%v error=%v", renewed, err)
	}
	next, err := TLSCertificate(dir)
	if err != nil {
		t.Fatal(err)
	}
	pinAfter, _ := CertificatePin(next)
	if !bytes.Equal(pinBefore, pinAfter) {
		t.Fatal("renewal changed identity pin")
	}
	leaf, _ := x509.ParseCertificate(next.Certificate[0])
	if leaf.NotAfter.Sub(time.Now()) < 360*24*time.Hour {
		t.Fatal("renewal did not extend certificate validity")
	}
	status, _ := post(t, client, server.URL+"/v1/selfhost/inspect", secret, InspectRequest{Nonce: "AAAAAAAAAAAAAAAAAAAAAA"})
	if status != 200 {
		t.Fatalf("live TLS did not reload valid certificate: status %d", status)
	}
	changed, err := RenewCertificate(dir, time.Now())
	if err != nil || changed {
		t.Fatal("fresh certificate renewed again")
	}
}
func TestCertificateRenewalFailureRetainsCurrentCertificate(t *testing.T) {
	dir, _, _, server := testInstallation(t, false)
	server.Close()
	s, err := load(dir)
	if err != nil {
		t.Fatal(err)
	}
	key, err := privateKey(s)
	if err != nil {
		t.Fatal(err)
	}
	old, err := certificatePEM(key, s.Config.EndpointHost, time.Now().AddDate(-1, 0, 15))
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(dir, "tls.crt"), old); err != nil {
		t.Fatal(err)
	}
	s.PrivateKey = "invalid"
	if err := save(dir, s); err != nil {
		t.Fatal(err)
	}
	changed, err := RenewCertificate(dir, time.Now())
	if err == nil || changed {
		t.Fatal("invalid identity was accepted for renewal")
	}
	after, err := os.ReadFile(filepath.Join(dir, "tls.crt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(old, after) {
		t.Fatal("failed renewal replaced current certificate")
	}
}
