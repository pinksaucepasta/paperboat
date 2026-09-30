package selfhost

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Config struct {
	Name          string      `json:"name"`
	EndpointHost  string      `json:"endpoint_host"`
	Listen        string      `json:"listen"`
	Components    []Component `json:"components"`
	PreviewDomain string      `json:"preview_domain,omitempty"`
	TunnelDomain  string      `json:"tunnel_domain,omitempty"`
	RuntimeDomain string      `json:"runtime_domain,omitempty"`
	ControlCA     string      `json:"control_ca,omitempty"`
}
type state struct {
	Version         int           `json:"version"`
	Config          Config        `json:"config"`
	PrivateKey      string        `json:"private_key"`
	UsagePrivateKey string        `json:"usage_private_key,omitempty"`
	SecretHash      string        `json:"secret_hash,omitempty"`
	ExpiresAt       int64         `json:"expires_at,omitempty"`
	ClaimHash       string        `json:"claim_hash,omitempty"`
	Claim           *ClaimRequest `json:"claim,omitempty"`
	Receipt         *Receipt      `json:"receipt,omitempty"`
	RuntimeReady    bool          `json:"runtime_ready"`
}

func validateConfig(c Config) error {
	if strings.TrimSpace(c.Name) == "" || c.Name != strings.TrimSpace(c.Name) || len(c.Name) > 80 || strings.ContainsAny(c.Name, "\r\n\x00") {
		return errors.New("installation name must contain 1 to 80 characters")
	}
	if c.EndpointHost == "" || strings.ContainsAny(c.EndpointHost, "/@ ") || (net.ParseIP(c.EndpointHost) == nil && strings.Contains(c.EndpointHost, ":")) {
		return errors.New("endpoint host must be a DNS name or IP without a port")
	}
	if _, _, err := net.SplitHostPort(c.Listen); err != nil {
		return errors.New("claim listen address requires host:port")
	}
	if len(c.Components) < 1 || len(c.Components) > 2 {
		return errors.New("choose relay, tunnel, or both")
	}
	seen := map[string]bool{}
	ports := map[int]bool{}
	for _, v := range c.Components {
		if (v.Capability != "relay" && v.Capability != "tunnel") || seen[v.Capability] {
			return errors.New("invalid or duplicate capability")
		}
		seen[v.Capability] = true
		limit := int64(256)
		if v.CapacityLimit < 1 || v.CapacityLimit > limit || v.Region == "" || v.FailureDomain == "" {
			return errors.New("invalid capacity, region, or failure domain")
		}
		for _, p := range []int{v.TCPPort, v.QUICPort} {
			if p < 1 || p > 65535 || ports[p] || p == ClaimPort {
				return errors.New("component ports must be distinct and must not use claim port 8443")
			}
			ports[p] = true
		}
		if v.Capability == "tunnel" && (v.TCPPort == 80 || v.TCPPort == 443 || v.QUICPort == 80 || v.QUICPort == 443) {
			return errors.New("tunnel carrier ports must not use public HTTP/HTTPS ports")
		}
	}
	return nil
}

// Initialize creates only a local identity and TLS key. It never registers ownership.
func Initialize(dir string, c Config) error {
	if !filepath.IsAbs(dir) {
		return errors.New("state directory must be absolute")
	}
	if err := validateConfig(c); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	lock, err := lockState(dir)
	if err != nil {
		return err
	}
	defer lock.Close()
	if existing, err := load(dir); err == nil {
		a, _ := json.Marshal(existing.Config)
		b, _ := json.Marshal(c)
		if string(a) != string(b) {
			return errors.New("installation already initialized with different settings")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	s := state{Version: 1, Config: c, PrivateKey: encoding.EncodeToString(key)}
	for _, comp := range c.Components {
		if comp.Capability == "tunnel" {
			_, usage, e := ed25519.GenerateKey(rand.Reader)
			if e != nil {
				return e
			}
			s.UsagePrivateKey = encoding.EncodeToString(usage)
		}
	}
	if err := writeCertificate(dir, key, c.EndpointHost); err != nil {
		return err
	}
	return save(dir, s)
}
func load(dir string) (state, error) {
	var s state
	path := filepath.Join(dir, "selfhost.json")
	info, err := os.Lstat(path)
	if err != nil {
		return s, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return s, errors.New("installation state must be a private regular file")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	if len(b) > 2<<20 || json.Unmarshal(b, &s) != nil || s.Version != 1 {
		return s, errors.New("invalid installation state")
	}
	return s, nil
}
func save(dir string, s state) error { return writeJSON(filepath.Join(dir, "selfhost.json"), s) }
func privateKey(s state) (ed25519.PrivateKey, error) {
	b, err := encoding.DecodeString(s.PrivateKey)
	if err != nil || len(b) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid installation key")
	}
	return ed25519.PrivateKey(b), nil
}
func TLSCertificate(dir string) (tls.Certificate, error) {
	return tls.LoadX509KeyPair(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"))
}

// CreateCode replaces any outstanding unclaimed code. The secret is never saved.
func CreateCode(dir string) (code, address string, expiry time.Time, err error) {
	lock, err := lockState(dir)
	if err != nil {
		return
	}
	defer lock.Close()
	s, err := load(dir)
	if err != nil {
		return
	}
	if s.Claim != nil {
		return "", "", time.Time{}, errors.New("installation is already claimed; manage it in the dashboard")
	}
	secret := make([]byte, 32)
	if _, err = rand.Read(secret); err != nil {
		return
	}
	sum := sha256.Sum256(secret)
	s.SecretHash = encoding.EncodeToString(sum[:])
	expiry = time.Now().Add(CodeTTL)
	s.ExpiresAt = expiry.Unix()
	cert, e := TLSCertificate(dir)
	if e != nil {
		err = e
		return
	}
	pin, e := CertificatePin(cert)
	if e != nil {
		err = e
		return
	}
	if err = save(dir, s); err != nil {
		return
	}
	_, port, _ := net.SplitHostPort(s.Config.Listen)
	address = net.JoinHostPort(s.Config.EndpointHost, port)
	code = fmt.Sprintf("pbh1.%s.%s", encoding.EncodeToString(pin), encoding.EncodeToString(secret))
	return
}
