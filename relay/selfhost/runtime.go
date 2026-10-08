package selfhost

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type Setup struct {
	ControlURL, Name, Capability, EndpointHost, Region, FailureDomain, ListenHost string
	TLSCert, TLSKey, ControlCA, PreviewDomain, TunnelDomain, RuntimeDomain        string
	TCPPort, QUICPort                                                             int
	CapacityLimit                                                                 int64
}
type Registration struct {
	InstallationID    string `json:"installation_id"`
	NodeID            string `json:"node_id"`
	RuntimeCredential string `json:"runtime_credential"`
	NodeGeneration    uint64 `json:"node_generation"`
	UsageKeyID        string `json:"usage_key_id,omitempty"`
	EdgePool          string `json:"edge_pool,omitempty"`
}

// ExportRuntime prepares protected files for an already scope-authorized node.
func ExportRuntime(ctx context.Context, dir string, s Setup, r Registration, usage ed25519.PrivateKey, jwks, revocations json.RawMessage) error {
	if !json.Valid(jwks) {
		return errors.New("invalid issuer JWKS")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(dir, "runtime.credential"), []byte(r.RuntimeCredential+"\n")); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(dir, "jwks.json"), jwks); err != nil {
		return err
	}
	return exportRuntime(ctx, dir, s, r, usage, revocations)
}
func exportRuntime(ctx context.Context, dir string, s Setup, r Registration, usage ed25519.PrivateKey, revocations json.RawMessage) error {
	var args []string
	if s.Capability == "relay" {
		args = []string{"-listen", listenAddress(s.ListenHost, s.QUICPort), "-wss-listen", listenAddress(s.ListenHost, s.TCPPort), "-tls-cert", s.TLSCert, "-tls-key", s.TLSKey, "-jwks", filepath.Join(dir, "jwks.json"), "-issuer", s.ControlURL, "-node-id", r.NodeID, "-node-generation", strconv.FormatUint(r.NodeGeneration, 10), "-control-url", s.ControlURL, "-control-credential-file", filepath.Join(dir, "runtime.credential"), "-node-state", filepath.Join(filepath.Dir(dir), "state", "node-state.json")}
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
		if !json.Valid(revocations) {
			return errors.New("invalid revocations document")
		}
		if err := writeFile(filepath.Join(dir, "revocations.json"), revocations); err != nil {
			return err
		}
		deployment := map[string]any{"self_hosted": true, "browser_access_enabled": false, "browser_login_origin": "", "control_url": s.ControlURL, "credential_issuer": s.ControlURL, "control_credential_file": filepath.Join(dir, "runtime.credential"), "control_ca_file": s.ControlCA, "jwks_file": filepath.Join(dir, "jwks.json"), "revocations_file": filepath.Join(dir, "revocations.json"), "usage_signing_key_file": filepath.Join(dir, "usage.key"), "infrastructure_tls_cert_file": s.TLSCert, "infrastructure_tls_key_file": s.TLSKey, "connector_advertise_host": s.EndpointHost, "carrier_tcp_listen_address": listenAddress(s.ListenHost, s.TCPPort), "carrier_quic_listen_address": listenAddress(s.ListenHost, s.QUICPort), "public_https_listen_address": listenAddress(s.ListenHost, 443), "private_https_listen_address": "127.0.0.1:9443", "public_http_listen_address": listenAddress(s.ListenHost, 80), "preview_base_domain": s.PreviewDomain, "tunnel_base_domain": s.TunnelDomain, "runtime_base_domain": s.RuntimeDomain, "trusted_proxy_cidrs": []string{}, "public_routes": []any{}, "node_capacity": s.CapacityLimit, "control_interval": int64(5 * time.Second), "control_timeout": int64(5 * time.Second), "max_body_bytes": int64(50 << 20), "max_header_bytes": int64(32 << 10)}
		if err := writeJSON(filepath.Join(dir, "deployment.json"), deployment); err != nil {
			return err
		}
		args = []string{"-node-id", r.NodeID, "-edge-pool", r.EdgePool, "-relay-id", r.NodeID, "-relay-region", s.Region, "-relay-name", s.Name, "-health-address", "127.0.0.1:9090", "-deployment-config", filepath.Join(dir, "deployment.json"), "-state-path", filepath.Join(filepath.Dir(dir), "state", "state.json")}
	}
	return writeJSON(filepath.Join(dir, "runtime.args"), args)
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
	if e != nil {
		return nil, true, fmt.Errorf("cannot read generated runtime arguments; finish claiming this installation in the dashboard: %w", e)
	}
	if len(out) == 0 {
		return nil, true, errors.New("cannot read generated runtime arguments; finish claiming this installation in the dashboard")
	}
	return out, true, nil
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

func listenAddress(host string, port int) string {
	if host == "" {
		host = "0.0.0.0"
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}
