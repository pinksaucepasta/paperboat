// Package selfhost implements the installation-local, pinned HTTPS claim protocol.
package selfhost

import (
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

const CodeTTL = 15 * time.Minute
const ClaimPort = 8443
const MaxBodyBytes = 1 << 20

var encoding = base64.RawURLEncoding.Strict()

type Component struct {
	Capability     string `json:"capability"`
	TCPPort        int    `json:"tcp_port"`
	QUICPort       int    `json:"quic_port"`
	Region         string `json:"region"`
	FailureDomain  string `json:"failure_domain"`
	CapacityLimit  int64  `json:"capacity_limit"`
	UsagePublicKey string `json:"usage_public_key,omitempty"`
}
type InspectRequest struct {
	Nonce string `json:"nonce"`
}
type Inspection struct {
	Version                      int         `json:"version"`
	Nonce                        string      `json:"nonce"`
	InstallationKey              string      `json:"installation_key"`
	Name                         string      `json:"name"`
	EndpointHost                 string      `json:"endpoint_host"`
	InfrastructureTLSCertificate string      `json:"infrastructure_tls_certificate"`
	ExpiresAt                    int64       `json:"expires_at"`
	Components                   []Component `json:"components"`
	Signature                    string      `json:"signature,omitempty"`
}
type RegisteredComponent struct {
	Capability string `json:"capability"`
	Registration
}
type ClaimRequest struct {
	ClaimID     string                `json:"claim_id"`
	ScopeType   string                `json:"scope_type"`
	ScopeID     string                `json:"scope_id"`
	ControlURL  string                `json:"control_url"`
	Components  []RegisteredComponent `json:"components"`
	JWKS        json.RawMessage       `json:"jwks"`
	Revocations json.RawMessage       `json:"revocations,omitempty"`
}
type ReceiptComponent struct {
	Capability     string `json:"capability"`
	InstallationID string `json:"installation_id"`
	NodeID         string `json:"node_id"`
	NodeGeneration uint64 `json:"node_generation"`
}
type Receipt struct {
	ClaimID         string             `json:"claim_id"`
	ScopeType       string             `json:"scope_type"`
	ScopeID         string             `json:"scope_id"`
	InstallationKey string             `json:"installation_key"`
	Components      []ReceiptComponent `json:"components"`
	Signature       string             `json:"signature,omitempty"`
}

func sign(key ed25519.PrivateKey, value any) string {
	b, _ := json.Marshal(value)
	return encoding.EncodeToString(ed25519.Sign(key, b))
}
func VerifyInspection(v Inspection) error {
	sig, err := encoding.DecodeString(v.Signature)
	if err != nil {
		return errors.New("invalid installation signature")
	}
	v.Signature = ""
	return verify(v.InstallationKey, v, sig)
}
func VerifyReceipt(v Receipt) error {
	sig, err := encoding.DecodeString(v.Signature)
	if err != nil {
		return errors.New("invalid installation signature")
	}
	v.Signature = ""
	return verify(v.InstallationKey, v, sig)
}
func verify(pub string, v any, sig []byte) error {
	key, err := encoding.DecodeString(pub)
	b, _ := json.Marshal(v)
	if err != nil || len(key) != ed25519.PublicKeySize || !ed25519.Verify(key, b, sig) {
		return errors.New("invalid installation signature")
	}
	return nil
}

// ParseCode returns the SPKI pin and bearer. Neither contains an account identity.
func ParseCode(code string) (pin, secret []byte, err error) {
	parts := strings.Split(strings.TrimSpace(code), ".")
	if len(parts) != 3 || parts[0] != "pbh1" {
		return nil, nil, errors.New("invalid self-host code")
	}
	pin, err = encoding.DecodeString(parts[1])
	if err != nil || len(pin) != 32 {
		return nil, nil, errors.New("invalid self-host code")
	}
	secret, err = encoding.DecodeString(parts[2])
	if err != nil || len(secret) != 32 {
		return nil, nil, errors.New("invalid self-host code")
	}
	return
}

// PinnedClient trusts only the bootstrap public key carried in the local code.
func PinnedClient(pin []byte) (*http.Client, error) {
	if len(pin) != 32 {
		return nil, errors.New("invalid TLS public-key pin")
	}
	expected := append([]byte(nil), pin...)
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true, VerifyConnection: func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) != 1 {
			return errors.New("unexpected bootstrap certificate chain")
		}
		cert := cs.PeerCertificates[0]
		sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
		if subtle.ConstantTimeCompare(sum[:], expected) != 1 {
			return errors.New("self-host TLS public-key pin mismatch")
		}
		if time.Now().Before(cert.NotBefore) || time.Now().After(cert.NotAfter) {
			return errors.New("bootstrap certificate expired")
		}
		return nil
	}}, MaxIdleConns: 2, IdleConnTimeout: 30 * time.Second, ResponseHeaderTimeout: 10 * time.Second, DisableKeepAlives: true}
	return &http.Client{Transport: transport, Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}
func CertificatePin(cert tls.Certificate) ([]byte, error) {
	if len(cert.Certificate) == 0 {
		return nil, errors.New("missing bootstrap certificate")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	return sum[:], nil
}
