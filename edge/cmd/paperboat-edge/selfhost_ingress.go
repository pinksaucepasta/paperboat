package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/edge/internal/config"
	"github.com/pinksaucepasta/paperboat/edge/internal/control"
)

func installationChallenge(node, host, credential string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := r.Host
		if h, _, err := net.SplitHostPort(name); err == nil {
			name = h
		}
		if !strings.EqualFold(name, host) || r.URL.Path != "/.well-known/paperboat-installation" {
			next.ServeHTTP(w, r)
			return
		}
		query := r.URL.Query()
		values := query["challenge"]
		if r.Method != "GET" || len(r.URL.RawQuery) > 128 || len(query) != 1 || len(values) != 1 {
			http.Error(w, "invalid challenge", 400)
			return
		}
		raw, err := base64.RawURLEncoding.Strict().DecodeString(values[0])
		if err != nil || len(raw) != 32 {
			http.Error(w, "invalid challenge", 400)
			return
		}
		mac := hmac.New(sha256.New, []byte(credential))
		mac.Write([]byte("paperboat-installation/v1\n" + node + "\n" + values[0]))
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]string{"node_id": node, "challenge": values[0], "proof": base64.RawURLEncoding.EncodeToString(mac.Sum(nil))})
	})
}

// infrastructureTLS serves the enrolled carrier certificate on the endpoint host. No browser trusts it:
// browser terminals use managed edges, and the control plane pins this identity (tls_spki_sha256) when it
// verifies ingress. Named publication routes keep their own distributed certificates.
func infrastructureTLS(host string, identity tls.Certificate, base *tls.Config) *tls.Config {
	result := base.Clone()
	fallback := result.GetCertificate
	result.GetCertificate = func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		if hello.ServerName == "" || strings.EqualFold(hello.ServerName, host) {
			return &identity, nil
		}
		return fallback(hello)
	}
	return result
}

// carrierServerTrust binds self-hosted H2/H3 carriers to the identity claimed
// during enrollment. Process epochs and signed admissions fence restarts.
func carrierServerTrust(d config.Deployment, nodeID, epoch, host string, now time.Time) (control.ProcessCarrierServerTrust, error) {
	certificate, err := tls.LoadX509KeyPair(d.InfrastructureTLSCertFile, d.InfrastructureTLSKeyFile)
	if err != nil {
		return control.ProcessCarrierServerTrust{}, fmt.Errorf("load claimed carrier TLS certificate and key: %w", err)
	}
	pin, err := control.CarrierServerSPKISHA256(certificate)
	if err != nil {
		return control.ProcessCarrierServerTrust{}, err
	}
	chain, err := control.CarrierServerCertificateChainPEM(certificate)
	if err != nil {
		return control.ProcessCarrierServerTrust{}, err
	}
	if err = control.ValidateCarrierServerCertificateChain(chain, pin, host, now); err != nil {
		return control.ProcessCarrierServerTrust{}, err
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return control.ProcessCarrierServerTrust{}, err
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	if _, err = leaf.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		return control.ProcessCarrierServerTrust{}, fmt.Errorf("verify claimed carrier TLS server-auth usage: %w", err)
	}
	return control.ProcessCarrierServerTrust{Certificate: certificate, SPKISHA256: pin, CertificateChainPEM: chain}, nil
}
