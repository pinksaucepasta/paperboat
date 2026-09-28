// Package operator implements local administration for a self-hosted Paperboat
// data-plane installation. It deliberately depends only on the standard library
// so relay and tunnel images can share one implementation.
package operator

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Setup struct {
	ControlURL, Name, Capability, EndpointHost, Region, FailureDomain      string
	TLSCert, TLSKey, ControlCA, PreviewDomain, TunnelDomain, RuntimeDomain string
	TCPPort, QUICPort                                                      int
	CapacityLimit                                                          int64
}

type State struct {
	Setup          Setup  `json:"setup"`
	Version        int    `json:"version"`
	ControlURL     string `json:"control_url"`
	ControlCA      string `json:"control_ca,omitempty"`
	InstallationID string `json:"installation_id"`
	NodeID         string `json:"node_id"`
	NodeGeneration uint64 `json:"node_generation"`
	Capability     string `json:"capability"`
	UsageKeyID     string `json:"usage_key_id,omitempty"`
	EdgePool       string `json:"edge_pool,omitempty"`
	PrivateKey     string `json:"operator_private_key"`
}

type InstallationRequest struct {
	Challenge      string `json:"challenge"`
	PublicKey      string `json:"public_key"`
	Name           string `json:"name"`
	Capability     string `json:"capability"`
	EndpointHost   string `json:"endpoint_host"`
	TCPPort        int    `json:"tcp_port"`
	QUICPort       int    `json:"quic_port"`
	Region         string `json:"region"`
	FailureDomain  string `json:"failure_domain"`
	CapacityLimit  int64  `json:"capacity_limit"`
	UsagePublicKey string `json:"usage_public_key,omitempty"`
	Signature      string `json:"signature,omitempty"`
}

type Registration struct {
	InstallationID    string `json:"installation_id"`
	NodeID            string `json:"node_id"`
	RuntimeCredential string `json:"runtime_credential"`
	NodeGeneration    uint64 `json:"node_generation"`
	UsageKeyID        string `json:"usage_key_id,omitempty"`
	EdgePool          string `json:"edge_pool,omitempty"`
}

type AdminResult struct {
	Code              string `json:"code,omitempty"`
	ExpiresAt         int64  `json:"expires_at,omitempty"`
	RuntimeCredential string `json:"runtime_credential,omitempty"`
	UsageKeyID        string `json:"usage_key_id,omitempty"`
	Accounts          []struct {
		AccountID  string     `json:"account_id"`
		Capability string     `json:"capability"`
		RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	} `json:"accounts,omitempty"`
}

const (
	maximumRelayCapacity  = 256
	maximumTunnelCapacity = 10000
)

func validate(s Setup) (Setup, error) {
	u, err := url.Parse(strings.TrimSpace(s.ControlURL))
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return Setup{}, errors.New("control URL must be an absolute HTTPS URL without credentials, query, or fragment")
	}
	s.ControlURL = strings.TrimRight(u.String(), "/")
	s.Name, s.EndpointHost, s.Region, s.FailureDomain = strings.TrimSpace(s.Name), strings.TrimSpace(s.EndpointHost), strings.TrimSpace(s.Region), strings.TrimSpace(s.FailureDomain)
	if s.Name == "" || len(s.Name) > 100 {
		return Setup{}, errors.New("name must contain 1 to 100 characters")
	}
	if s.Capability != "relay" && s.Capability != "tunnel" {
		return Setup{}, errors.New("capability must be relay or tunnel")
	}
	if s.EndpointHost == "" || strings.ContainsAny(s.EndpointHost, "/@") || (net.ParseIP(s.EndpointHost) == nil && strings.Contains(s.EndpointHost, ":")) {
		return Setup{}, errors.New("endpoint host must be a DNS name or IP address without a port")
	}
	if s.TCPPort < 1 || s.TCPPort > 65535 || s.QUICPort < 1 || s.QUICPort > 65535 {
		return Setup{}, errors.New("TCP and QUIC ports must be between 1 and 65535")
	}
	if s.TCPPort == s.QUICPort {
		return Setup{}, errors.New("TCP/STUN and QUIC ports must be distinct")
	}
	if s.Capability == "tunnel" && (s.TCPPort == 80 || s.TCPPort == 443 || s.QUICPort == 80 || s.QUICPort == 443) {
		return Setup{}, errors.New("carrier TCP and QUIC ports must be distinct and must not collide with tunnel HTTP/HTTPS ports 80 and 443")
	}
	if s.Region == "" || s.FailureDomain == "" || s.CapacityLimit < 1 {
		return Setup{}, errors.New("region, failure domain, and positive capacity limit are required")
	}
	if s.Capability == "relay" && s.CapacityLimit > maximumRelayCapacity {
		return Setup{}, errors.New("relay capacity limit must not exceed 256")
	}
	if s.Capability == "tunnel" && s.CapacityLimit > maximumTunnelCapacity {
		return Setup{}, errors.New("tunnel capacity limit must not exceed 10000")
	}
	if !filepath.IsAbs(s.TLSCert) || !filepath.IsAbs(s.TLSKey) {
		return Setup{}, errors.New("absolute --tls-cert and --tls-key paths are required")
	}
	pair, err := tls.LoadX509KeyPair(s.TLSCert, s.TLSKey)
	if err != nil || len(pair.Certificate) == 0 {
		return Setup{}, errors.New("TLS certificate and private key are unreadable or do not match")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil || leaf.VerifyHostname(s.EndpointHost) != nil {
		return Setup{}, errors.New("TLS certificate must cover the registered endpoint host")
	}
	if s.ControlCA != "" && !filepath.IsAbs(s.ControlCA) {
		return Setup{}, errors.New("--control-ca must be an absolute path")
	}
	if s.Capability == "tunnel" && !domainSetValid(s.PreviewDomain, s.TunnelDomain, s.RuntimeDomain) {
		return Setup{}, errors.New("tunnel setup requires three distinct non-overlapping DNS base domains")
	}
	return s, nil
}

// Locally replaceable certificate paths and runtime domains are not registry identity.
func sameRegistration(a, b Setup) bool {
	return a.ControlURL == b.ControlURL && a.Name == b.Name && a.Capability == b.Capability && a.EndpointHost == b.EndpointHost && a.Region == b.Region && a.FailureDomain == b.FailureDomain && a.TCPPort == b.TCPPort && a.QUICPort == b.QUICPort && a.CapacityLimit == b.CapacityLimit
}

func domainSetValid(values ...string) bool {
	for i, v := range values {
		v = strings.TrimSpace(v)
		if v == "" || net.ParseIP(v) != nil || !strings.Contains(v, ".") {
			return false
		}
		for j, o := range values {
			if i != j && (v == o || strings.HasSuffix(v, "."+o) || strings.HasSuffix(o, "."+v)) {
				return false
			}
		}
	}
	return true
}

// Register creates a durable operator identity, registers the installation,
// and atomically writes state and runtime bearer into the protected directory.
func Register(ctx context.Context, dir string, setup Setup, client *http.Client) (Registration, error) {
	setup, err := validate(setup)
	if err != nil {
		return Registration{}, err
	}
	if !filepath.IsAbs(dir) {
		return Registration{}, errors.New("state directory must be absolute")
	}
	if client == nil {
		client, err = setupHTTPClient(setup.ControlCA)
		if err != nil {
			return Registration{}, err
		}
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return Registration{}, fmt.Errorf("create state directory: %w", err)
	}
	if err = os.Chmod(dir, 0700); err != nil {
		return Registration{}, err
	}
	lock, err := lockOperator(dir)
	if err != nil {
		return Registration{}, err
	}
	defer lock.Close()
	statePath := filepath.Join(dir, "operator.json")
	var pub ed25519.PublicKey
	var priv ed25519.PrivateKey
	var usagePrivate ed25519.PrivateKey
	var usagePublic string
	var savedUsageKeyID string
	usagePending := filepath.Join(dir, "usage.pending")
	if setup.Capability == "tunnel" {
		if b, e := os.ReadFile(filepath.Join(dir, "usage.key")); e == nil {
			var saved struct {
				KeyID      string `json:"key_id"`
				PrivateKey string `json:"private_key"`
			}
			if json.Unmarshal(b, &saved) != nil || saved.KeyID == "" {
				return Registration{}, errors.New("invalid saved usage signing key")
			}
			raw, de := base64.RawURLEncoding.Strict().DecodeString(saved.PrivateKey)
			if de != nil || len(raw) != ed25519.PrivateKeySize {
				return Registration{}, errors.New("invalid saved usage signing key")
			}
			usagePrivate = ed25519.PrivateKey(raw)
			savedUsageKeyID = saved.KeyID
		} else if !errors.Is(e, os.ErrNotExist) {
			return Registration{}, e
		} else if b, e := os.ReadFile(usagePending); e == nil {
			raw, de := base64.RawURLEncoding.Strict().DecodeString(strings.TrimSpace(string(b)))
			if de != nil || len(raw) != ed25519.PrivateKeySize {
				return Registration{}, errors.New("invalid pending usage signing key")
			}
			usagePrivate = ed25519.PrivateKey(raw)
		} else if !errors.Is(e, os.ErrNotExist) {
			return Registration{}, e
		} else {
			p, q, e := ed25519.GenerateKey(rand.Reader)
			if e != nil {
				return Registration{}, e
			}
			usagePrivate = q
			if e = writeFile(usagePending, []byte(base64.RawURLEncoding.EncodeToString(q)+"\n")); e != nil {
				return Registration{}, e
			}
			_ = p
		}
		usagePublic = base64.RawURLEncoding.EncodeToString(usagePrivate.Public().(ed25519.PublicKey))
	}
	if existing, loadErr := Load(dir); loadErr == nil {
		if !sameRegistration(existing.Setup, setup) {
			return Registration{}, errors.New("registered node settings cannot be changed by regenerating local files; use the original settings or remove this installation and set up a new one")
		}
		if existing.InstallationID != "" {
			// usage.key is written first during rotation and is the recovery
			// authority if the following operator.json write was interrupted.
			if setup.Capability == "tunnel" && savedUsageKeyID != "" {
				existing.UsageKeyID = savedUsageKeyID
			}
			result := Registration{InstallationID: existing.InstallationID, NodeID: existing.NodeID, NodeGeneration: existing.NodeGeneration, UsageKeyID: existing.UsageKeyID, EdgePool: existing.EdgePool}
			if err = exportRuntime(ctx, dir, setup, result, usagePrivate, client); err != nil {
				return Registration{}, err
			}
			existing.Setup, existing.ControlCA = setup, setup.ControlCA
			if err = writeJSON(statePath, existing); err != nil {
				return Registration{}, err
			}
			_ = os.Remove(usagePending)
			return result, nil
		}
		decoded, decodeErr := base64.RawURLEncoding.Strict().DecodeString(existing.PrivateKey)
		if decodeErr != nil || len(decoded) != ed25519.PrivateKeySize {
			return Registration{}, errors.New("invalid pending operator identity")
		}
		priv = ed25519.PrivateKey(decoded)
		pub = priv.Public().(ed25519.PublicKey)
	} else if !errors.Is(loadErr, os.ErrNotExist) {
		return Registration{}, loadErr
	} else {
		pub, priv, err = ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return Registration{}, err
		}
		pending := State{Setup: setup, Version: 1, ControlURL: setup.ControlURL, ControlCA: setup.ControlCA, Capability: setup.Capability, PrivateKey: base64.RawURLEncoding.EncodeToString(priv)}
		if err = writeJSON(statePath, pending); err != nil {
			return Registration{}, fmt.Errorf("persist pending operator identity: %w", err)
		}
	}
	var ch struct {
		Challenge string `json:"challenge"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err = request(ctx, client, http.MethodPost, setup.ControlURL+"/v1/selfhost/challenges", map[string]string{"public_key": base64.RawURLEncoding.EncodeToString(pub)}, nil, &ch); err != nil {
		return Registration{}, err
	}
	if ch.Challenge == "" || ch.ExpiresAt <= time.Now().Unix() {
		return Registration{}, errors.New("control plane returned an invalid or expired registration challenge")
	}
	req := InstallationRequest{Challenge: ch.Challenge, PublicKey: base64.RawURLEncoding.EncodeToString(pub), Name: setup.Name, Capability: setup.Capability, EndpointHost: setup.EndpointHost, TCPPort: setup.TCPPort, QUICPort: setup.QUICPort, Region: setup.Region, FailureDomain: setup.FailureDomain, CapacityLimit: setup.CapacityLimit, UsagePublicKey: usagePublic}
	raw, _ := json.Marshal(req)
	req.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, raw))
	var result Registration
	if err = request(ctx, client, http.MethodPost, setup.ControlURL+"/v1/selfhost/installations", req, nil, &result); err != nil {
		return Registration{}, err
	}
	if result.InstallationID == "" || result.NodeID == "" || result.RuntimeCredential == "" || result.NodeGeneration == 0 {
		return Registration{}, errors.New("control plane returned an incomplete installation registration")
	}
	state := State{Setup: setup, Version: 1, ControlURL: setup.ControlURL, ControlCA: setup.ControlCA, InstallationID: result.InstallationID, NodeID: result.NodeID, NodeGeneration: result.NodeGeneration, Capability: setup.Capability, UsageKeyID: result.UsageKeyID, EdgePool: result.EdgePool, PrivateKey: base64.RawURLEncoding.EncodeToString(priv)}
	if err = writeFile(filepath.Join(dir, "runtime.credential"), []byte(result.RuntimeCredential+"\n")); err != nil {
		return Registration{}, err
	}
	if err = writeJSON(statePath, state); err != nil {
		_ = os.Remove(filepath.Join(dir, "runtime.credential"))
		return Registration{}, err
	}
	if err = exportRuntime(ctx, dir, setup, result, usagePrivate, client); err != nil {
		return Registration{}, err
	}
	_ = os.Remove(usagePending)
	return result, nil
}

func setupHTTPClient(caPath string) (*http.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if caPath != "" {
		b, e := os.ReadFile(caPath)
		if e != nil {
			return nil, fmt.Errorf("read control CA: %w", e)
		}
		pool, e := x509.SystemCertPool()
		if e != nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(b) {
			return nil, errors.New("control CA contains no certificates")
		}
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}
	}
	return &http.Client{Timeout: 15 * time.Second, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, nil
}

func exportRuntime(ctx context.Context, dir string, s Setup, r Registration, usage ed25519.PrivateKey, c *http.Client) error {
	if err := download(ctx, c, s.ControlURL+"/.well-known/jwks.json", "", filepath.Join(dir, "jwks.json")); err != nil {
		return fmt.Errorf("fetch issuer JWKS: %w", err)
	}
	var args []string
	if s.Capability == "relay" {
		args = []string{"-listen", fmt.Sprintf("0.0.0.0:%d", s.QUICPort), "-wss-listen", fmt.Sprintf("0.0.0.0:%d", s.TCPPort), "-tls-cert", s.TLSCert, "-tls-key", s.TLSKey, "-jwks", filepath.Join(dir, "jwks.json"), "-issuer", s.ControlURL, "-node-id", r.NodeID, "-node-generation", strconv.FormatUint(r.NodeGeneration, 10), "-control-url", s.ControlURL, "-control-credential-file", filepath.Join(dir, "runtime.credential"), "-node-state", filepath.Join(filepath.Dir(dir), "state", "node-state.json")}
		if s.ControlCA != "" {
			args = append(args, "-control-ca", s.ControlCA)
		}
	} else {
		if r.UsageKeyID == "" || r.EdgePool == "" {
			return errors.New("control plane omitted tunnel usage key or edge pool")
		}
		keyDoc := map[string]string{"key_id": r.UsageKeyID, "edge_node_id": r.NodeID, "private_key": base64.RawURLEncoding.EncodeToString(usage)}
		if err := writeJSON(filepath.Join(dir, "usage.key"), keyDoc); err != nil {
			return err
		}
		credentialRaw, e := os.ReadFile(filepath.Join(dir, "runtime.credential"))
		if e != nil {
			return e
		}
		if err := download(ctx, c, s.ControlURL+"/v1/trust/revocations", strings.TrimSpace(string(credentialRaw)), filepath.Join(dir, "revocations.json")); err != nil {
			return fmt.Errorf("fetch revocations: %w", err)
		}
		deployment := map[string]any{"browser_access_enabled": false, "browser_login_origin": "", "control_url": s.ControlURL, "credential_issuer": s.ControlURL, "control_credential_file": filepath.Join(dir, "runtime.credential"), "control_ca_file": s.ControlCA, "jwks_file": filepath.Join(dir, "jwks.json"), "revocations_file": filepath.Join(dir, "revocations.json"), "usage_signing_key_file": filepath.Join(dir, "usage.key"), "infrastructure_tls_cert_file": s.TLSCert, "infrastructure_tls_key_file": s.TLSKey, "connector_advertise_host": s.EndpointHost, "carrier_tcp_listen_address": fmt.Sprintf("0.0.0.0:%d", s.TCPPort), "carrier_quic_listen_address": fmt.Sprintf("0.0.0.0:%d", s.QUICPort), "public_https_listen_address": "0.0.0.0:443", "private_https_listen_address": "127.0.0.1:9443", "public_http_listen_address": "0.0.0.0:80", "preview_base_domain": s.PreviewDomain, "tunnel_base_domain": s.TunnelDomain, "runtime_base_domain": s.RuntimeDomain, "trusted_proxy_cidrs": []string{}, "public_routes": []any{}, "node_capacity": s.CapacityLimit, "control_interval": int64(5 * time.Second), "control_timeout": int64(5 * time.Second), "max_body_bytes": int64(50 << 20), "max_header_bytes": int64(32 << 10)}
		if err := writeJSON(filepath.Join(dir, "deployment.json"), deployment); err != nil {
			return err
		}
		args = []string{"-node-id", r.NodeID, "-edge-pool", r.EdgePool, "-relay-id", r.NodeID, "-relay-region", s.Region, "-relay-name", s.Name, "-health-address", "127.0.0.1:9090", "-deployment-config", filepath.Join(dir, "deployment.json"), "-state-path", filepath.Join(filepath.Dir(dir), "state", "state.json")}
	}
	return writeJSON(filepath.Join(dir, "runtime.args"), args)
}

func download(ctx context.Context, c *http.Client, target, bearer, path string) error {
	req, e := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if e != nil {
		return e
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, e := c.Do(req)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, e := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if e != nil || len(b) == 0 {
		return errors.New("empty or unreadable trust document")
	}
	var doc any
	if json.Unmarshal(b, &doc) != nil {
		return errors.New("trust document is not JSON")
	}
	return writeFile(path, b)
}

func RuntimeArgs(args []string) ([]string, bool, error) {
	if len(args) != 3 || args[0] != "run" || args[1] != "--state-dir" {
		return args, false, nil
	}
	if !filepath.IsAbs(args[2]) {
		return nil, true, errors.New("runtime state directory must be absolute")
	}
	var out []string
	b, e := os.ReadFile(filepath.Join(args[2], "runtime.args"))
	if e == nil {
		e = json.Unmarshal(b, &out)
	}
	if e != nil || len(out) == 0 {
		return nil, true, errors.New("cannot read generated runtime arguments; rerun operator setup")
	}
	return out, true, nil
}

func Load(dir string) (State, error) {
	var s State
	path := filepath.Join(dir, "operator.json")
	info, err := os.Lstat(path)
	if err != nil {
		return s, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return s, errors.New("operator state must be a private regular file")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	if len(b) > 16<<10 || json.Unmarshal(b, &s) != nil || s.Version != 1 {
		return State{}, errors.New("invalid operator state")
	}
	return s, nil
}

func Admin(ctx context.Context, dir, action, accountID string, client *http.Client) (AdminResult, error) {
	lock, err := lockOperator(dir)
	if err != nil {
		return AdminResult{}, err
	}
	defer lock.Close()
	s, err := Load(dir)
	if err != nil {
		return AdminResult{}, err
	}
	if client == nil {
		client, err = setupHTTPClient(s.ControlCA)
		if err != nil {
			return AdminResult{}, err
		}
	}
	key, err := base64.RawURLEncoding.Strict().DecodeString(s.PrivateKey)
	if err != nil || len(key) != ed25519.PrivateKeySize {
		return AdminResult{}, errors.New("invalid operator private key")
	}
	path := "/v1/selfhost/installations/" + url.PathEscape(s.InstallationID) + "/admin"
	usagePublic := ""
	var nextUsage ed25519.PrivateKey
	if action == "rotate-usage" {
		if s.Capability != "tunnel" {
			return AdminResult{}, errors.New("usage signing keys apply only to tunnel installations")
		}
		pending := filepath.Join(dir, "usage.pending")
		if raw, e := os.ReadFile(pending); e == nil {
			decoded, de := base64.RawURLEncoding.Strict().DecodeString(strings.TrimSpace(string(raw)))
			if de != nil || len(decoded) != ed25519.PrivateKeySize {
				return AdminResult{}, errors.New("invalid pending usage signing key")
			}
			nextUsage = ed25519.PrivateKey(decoded)
		} else if !errors.Is(e, os.ErrNotExist) {
			return AdminResult{}, e
		} else {
			_, nextUsage, err = ed25519.GenerateKey(rand.Reader)
			if err != nil {
				return AdminResult{}, err
			}
			if err = writeFile(pending, []byte(base64.RawURLEncoding.EncodeToString(nextUsage)+"\n")); err != nil {
				return AdminResult{}, err
			}
		}
		usagePublic = base64.RawURLEncoding.EncodeToString(nextUsage.Public().(ed25519.PublicKey))
	}
	body, _ := json.Marshal(struct {
		Action         string `json:"action"`
		AccountID      string `json:"account_id,omitempty"`
		UsagePublicKey string `json:"usage_public_key,omitempty"`
	}{Action: action, AccountID: strings.TrimSpace(accountID), UsagePublicKey: usagePublic})
	nonce := make([]byte, 24)
	if _, err = rand.Read(nonce); err != nil {
		return AdminResult{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	sum := sha256.Sum256(body)
	proof := ed25519.Sign(ed25519.PrivateKey(key), []byte(http.MethodPost+"\n"+path+"\n"+now+"\n"+base64.RawURLEncoding.EncodeToString(nonce)+"\n"+hex.EncodeToString(sum[:])))
	head := http.Header{"X-Paperboat-Operator-Time": []string{now}, "X-Paperboat-Operator-Nonce": []string{base64.RawURLEncoding.EncodeToString(nonce)}, "X-Paperboat-Operator-Proof": []string{base64.RawURLEncoding.EncodeToString(proof)}}
	var out AdminResult
	if err = requestRaw(ctx, client, http.MethodPost, s.ControlURL+path, body, head, &out); err != nil {
		return AdminResult{}, err
	}
	if action == "rotate" {
		if out.RuntimeCredential == "" {
			return AdminResult{}, errors.New("control plane returned no rotated runtime credential")
		}
		if err = writeFile(filepath.Join(dir, "runtime.credential"), []byte(out.RuntimeCredential+"\n")); err != nil {
			return AdminResult{}, err
		}
	}
	if action == "rotate-usage" {
		if out.UsageKeyID == "" {
			return AdminResult{}, errors.New("control plane returned no rotated usage key ID")
		}
		if err = writeJSON(filepath.Join(dir, "usage.key"), map[string]string{"key_id": out.UsageKeyID, "edge_node_id": s.NodeID, "private_key": base64.RawURLEncoding.EncodeToString(nextUsage)}); err != nil {
			return AdminResult{}, err
		}
		s.UsageKeyID = out.UsageKeyID
		if err = writeJSON(filepath.Join(dir, "operator.json"), s); err != nil {
			return AdminResult{}, fmt.Errorf("persist rotated usage key state: %w", err)
		}
		_ = os.Remove(filepath.Join(dir, "usage.pending"))
	}
	if action == "remove" {
		for _, name := range []string{"runtime.credential", "jwks.json", "revocations.json", "usage.key", "usage.pending", "deployment.json", "runtime.args", "operator.json"} {
			if err = os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return AdminResult{}, fmt.Errorf("remove %s: %w", name, err)
			}
		}
	}
	return out, nil
}

func request(ctx context.Context, c *http.Client, method, target string, in any, h http.Header, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return requestRaw(ctx, c, method, target, b, h, out)
}
func requestRaw(ctx context.Context, c *http.Client, method, target string, b []byte, h http.Header, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range h {
		req.Header[k] = v
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("contact control plane: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Error.Message != "" {
			return errors.New(e.Error.Message)
		}
		return fmt.Errorf("control plane returned HTTP %d", resp.StatusCode)
	}
	if out != nil && json.Unmarshal(data, out) != nil {
		return errors.New("control plane returned invalid JSON")
	}
	return nil
}
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(path, append(b, '\n'))
}
func writeFile(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".paperboat-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(b)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err == nil {
		parent, e := os.Open(filepath.Dir(path))
		if e != nil {
			return e
		}
		defer parent.Close()
		err = parent.Sync()
	}
	return err
}

// ParseSetup converts command flag values without importing a CLI framework.
func ParsePort(v string) (int, error) {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 65535 {
		return 0, errors.New("port must be between 1 and 65535")
	}
	return n, nil
}
