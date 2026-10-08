package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat-tunnel/internal/config"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/control"
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

// The optional operator certificate is valid only for the installation's
// infrastructure hostname and no-SNI IP connections. Named managed routes
// continue to use their own certificates.
func infrastructureTLS(d config.Deployment, base *tls.Config) (*tls.Config, error) {
	if d.InfrastructureTLSCertFile == "" && d.InfrastructureTLSKeyFile == "" {
		return base, nil
	}
	if d.InfrastructureTLSCertFile == "" || d.InfrastructureTLSKeyFile == "" {
		return nil, errors.New("infrastructure TLS needs both certificate and key files")
	}
	certificate, err := tls.LoadX509KeyPair(d.InfrastructureTLSCertFile, d.InfrastructureTLSKeyFile)
	if err != nil {
		return nil, errors.New("cannot load infrastructure TLS certificate and key")
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil || leaf.VerifyHostname(d.ConnectorAdvertiseHost) != nil || time.Now().Before(leaf.NotBefore) || !time.Now().Before(leaf.NotAfter) {
		return nil, errors.New("infrastructure TLS certificate must currently cover the installation hostname")
	}
	result := base.Clone()
	fallback := result.GetCertificate
	result.GetCertificate = func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		if hello.ServerName == "" || strings.EqualFold(hello.ServerName, d.ConnectorAdvertiseHost) {
			return &certificate, nil
		}
		return fallback(hello)
	}
	return result, nil
}

// carrierServerTrust binds self-hosted H2/H3 carriers to the identity claimed
// during enrollment. Process epochs and signed admissions fence restarts.
func carrierServerTrust(d config.Deployment, nodeID, epoch, host string, now time.Time) (control.ProcessCarrierServerTrust, error) {
	if !d.SelfHosted {
		return control.NewProcessCarrierServerTrust(nodeID, epoch, host, now, control.DefaultProcessCarrierServerCertificateLifetime)
	}
	certificate, err := tls.LoadX509KeyPair(d.InfrastructureTLSCertFile, d.InfrastructureTLSKeyFile)
	if err != nil {
		return control.ProcessCarrierServerTrust{}, errors.New("cannot load claimed carrier TLS certificate and key")
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
		return control.ProcessCarrierServerTrust{}, errors.New("claimed carrier TLS certificate requires server-auth usage")
	}
	return control.ProcessCarrierServerTrust{Certificate: certificate, SPKISHA256: pin, CertificateChainPEM: chain}, nil
}
