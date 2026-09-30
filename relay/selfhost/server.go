package selfhost

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Handler performs each state transition under the installation's process lock.
func Handler(dir string, onClaim func()) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/selfhost/inspect", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req InspectRequest
		if err := decode(w, r, &req); err != nil {
			return
		}
		nonce, err := encoding.DecodeString(req.Nonce)
		if err != nil || len(nonce) < 16 || len(nonce) > 64 {
			http.Error(w, "invalid inspection nonce", 400)
			return
		}
		lock, err := lockState(dir)
		if err != nil {
			http.Error(w, "installation busy; retry", 503)
			return
		}
		defer lock.Close()
		s, err := load(dir)
		if err != nil {
			http.Error(w, "installation state unavailable", 503)
			return
		}
		if !authorized(r, s) {
			http.Error(w, "code invalid", http.StatusUnauthorized)
			return
		}
		if s.Claim == nil && time.Now().Unix() >= s.ExpiresAt {
			http.Error(w, "code expired", http.StatusGone)
			return
		}

		key, err := privateKey(s)
		if err != nil {
			http.Error(w, "installation identity unavailable", 503)
			return
		}
		out := Inspection{Version: 1, Nonce: req.Nonce, InstallationKey: encoding.EncodeToString(key.Public().(ed25519.PublicKey)), Name: s.Config.Name, EndpointHost: s.Config.EndpointHost, ExpiresAt: s.ExpiresAt, Components: append([]Component(nil), s.Config.Components...)}
		certPEM, err := os.ReadFile(filepath.Join(dir, "tls.crt"))
		if err != nil {
			http.Error(w, "installation certificate unavailable", 503)
			return
		}
		out.InfrastructureTLSCertificate = string(certPEM)

		if s.UsagePrivateKey != "" {
			usage, e := encoding.DecodeString(s.UsagePrivateKey)
			if e != nil || len(usage) != ed25519.PrivateKeySize {
				http.Error(w, "usage identity unavailable", 503)
				return
			}
			for i := range out.Components {
				if out.Components[i].Capability == "tunnel" {
					out.Components[i].UsagePublicKey = encoding.EncodeToString(ed25519.PrivateKey(usage).Public().(ed25519.PublicKey))
				}
			}
		}
		out.Signature = sign(key, out)
		respond(w, out)
	})
	mux.HandleFunc("/v1/selfhost/claim", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req ClaimRequest
		if err := decode(w, r, &req); err != nil {
			return
		}
		lock, err := lockState(dir)
		if err != nil {
			http.Error(w, "installation busy; retry", 503)
			return
		}
		defer lock.Close()
		s, err := load(dir)
		if err != nil {
			http.Error(w, "installation state unavailable", 503)
			return
		}
		if !authorized(r, s) {
			http.Error(w, "code invalid", 401)
			return
		}
		req.ControlURL = strings.TrimRight(req.ControlURL, "/")
		identity := req
		identity.JWKS = nil
		identity.Revocations = nil
		b, _ := json.Marshal(identity)
		sum := sha256.Sum256(b)
		hash := encoding.EncodeToString(sum[:])
		if s.Claim != nil {
			if s.ClaimHash != hash {
				http.Error(w, "code has already been claimed", 409)
				return
			}
		} else {
			if time.Now().Unix() >= s.ExpiresAt {
				http.Error(w, "code expired", http.StatusGone)
				return
			}
			if err := validateClaim(s.Config, req); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			s.Claim = &req
			s.ClaimHash = hash
			if err := save(dir, s); err != nil {
				http.Error(w, "could not persist claim; retry same claim", 503)
				return
			}
		}
		if !s.RuntimeReady {
			if err := applyClaim(r.Context(), dir, &s); err != nil {
				http.Error(w, "claim saved; runtime configuration incomplete; retry same claim", 503)
				return
			}
		}
		respond(w, s.Receipt)
		if onClaim != nil {
			onClaim()
		}
	})
	return mux
}
func authorized(r *http.Request, s state) bool {
	value := r.Header.Get("Authorization")
	if !strings.HasPrefix(value, "Bearer ") {
		return false
	}
	secret, err := encoding.DecodeString(strings.TrimPrefix(value, "Bearer "))
	if err != nil || len(secret) != 32 {
		return false
	}
	hash := sha256.Sum256(secret)
	expected, err := encoding.DecodeString(s.SecretHash)
	return err == nil && subtle.ConstantTimeCompare(hash[:], expected) == 1
}
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	defer r.Body.Close()
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	err := d.Decode(v)
	if err == nil {
		var extra any
		if d.Decode(&extra) != io.EOF {
			err = errors.New("extra JSON")
		}
	}
	if err != nil {
		http.Error(w, "invalid claim request", 400)
	}
	return err
}
func respond(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}
func validateClaim(c Config, r ClaimRequest) error {
	if r.ClaimID == "" || len(r.ClaimID) > 100 || r.ScopeID == "" || len(r.ScopeID) > 100 || (r.ScopeType != "account" && r.ScopeType != "team" && r.ScopeType != "global") {
		return errors.New("invalid claim identity or scope")
	}
	u, err := url.Parse(r.ControlURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return errors.New("control URL must be an HTTPS origin")
	}
	if len(r.Components) != len(c.Components) {
		return errors.New("claim must register every installed component")
	}
	installed := map[string]bool{}
	for _, v := range c.Components {
		installed[v.Capability] = true
	}
	seen := map[string]bool{}
	for _, v := range r.Components {
		if !installed[v.Capability] || seen[v.Capability] || v.InstallationID == "" || v.NodeID == "" || v.NodeGeneration == 0 || len(v.RuntimeCredential) < 32 || len(v.RuntimeCredential) > 4096 {
			return errors.New("invalid registered component")
		}
		seen[v.Capability] = true
		if v.Capability == "tunnel" && (v.UsageKeyID == "" || v.EdgePool == "" || !json.Valid(r.Revocations)) {
			return errors.New("tunnel claim requires usage identity, edge pool, and revocations")
		}
	}
	var jwks struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if json.Unmarshal(r.JWKS, &jwks) != nil || len(jwks.Keys) == 0 {
		return errors.New("claim requires issuer signing keys")
	}
	return nil
}
func applyClaim(ctx context.Context, dir string, s *state) error {
	key, err := privateKey(*s)
	if err != nil {
		return err
	}
	var usage ed25519.PrivateKey
	if s.UsagePrivateKey != "" {
		b, e := encoding.DecodeString(s.UsagePrivateKey)
		if e != nil || len(b) != ed25519.PrivateKeySize {
			return errors.New("invalid usage key")
		}
		usage = ed25519.PrivateKey(b)
	}
	receipt := Receipt{ClaimID: s.Claim.ClaimID, ScopeType: s.Claim.ScopeType, ScopeID: s.Claim.ScopeID, InstallationKey: encoding.EncodeToString(key.Public().(ed25519.PublicKey))}
	for _, registered := range s.Claim.Components {
		var comp Component
		for _, v := range s.Config.Components {
			if v.Capability == registered.Capability {
				comp = v
			}
		}
		listenHost, _, _ := net.SplitHostPort(s.Config.Listen)
		setup := Setup{ListenHost: listenHost, ControlURL: s.Claim.ControlURL, Name: s.Config.Name, Capability: comp.Capability, EndpointHost: s.Config.EndpointHost, Region: comp.Region, FailureDomain: comp.FailureDomain, TCPPort: comp.TCPPort, QUICPort: comp.QUICPort, CapacityLimit: comp.CapacityLimit, TLSCert: filepath.Join(dir, "tls.crt"), TLSKey: filepath.Join(dir, "tls.key"), ControlCA: s.Config.ControlCA, PreviewDomain: s.Config.PreviewDomain, TunnelDomain: s.Config.TunnelDomain, RuntimeDomain: s.Config.RuntimeDomain}
		componentDir := filepath.Join(dir, comp.Capability)
		if err := ExportRuntime(ctx, componentDir, setup, registered.Registration, usage, s.Claim.JWKS, s.Claim.Revocations); err != nil {
			return err
		}
		receipt.Components = append(receipt.Components, ReceiptComponent{Capability: comp.Capability, InstallationID: registered.InstallationID, NodeID: registered.NodeID, NodeGeneration: registered.NodeGeneration})
	}
	receipt.Signature = sign(key, receipt)
	s.Receipt = &receipt
	s.RuntimeReady = true
	return save(dir, *s)
}

// Serve exposes the bounded local claim API and drains it on cancellation.
func Serve(ctx context.Context, dir string, onClaim func()) error {
	lock, err := lockState(dir)
	if err != nil {
		return err
	}
	s, err := load(dir)
	if err == nil && s.Claim != nil && !s.RuntimeReady {
		err = applyClaim(ctx, dir, &s)
	}
	lock.Close()
	if err != nil {
		return err
	}
	if s.RuntimeReady && onClaim != nil {
		onClaim()
	}
	cert, err := TLSCertificate(dir)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", s.Config.Listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: Handler(dir, onClaim), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 20 * time.Second, IdleTimeout: 15 * time.Second, MaxHeaderBytes: 8 << 10, TLSConfig: liveTLSConfig(dir, cert)}
	done := make(chan error, 1)
	go func() { done <- srv.ServeTLS(listener, "", "") }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := srv.Shutdown(shutdown)
		if err != nil {
			srv.Close()
		}
		<-done
		return err
	}
}
func RuntimeReady(dir string) (bool, error) { s, err := load(dir); return s.RuntimeReady, err }
func ComponentDirectories(dir string) ([]string, error) {
	s, err := load(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, c := range s.Config.Components {
		out = append(out, filepath.Join(dir, c.Capability))
	}
	return out, nil
}
func tlsConfig(cert tls.Certificate) *tls.Config {
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}
}

// RemoveLocal deletes only files beneath a protected, stopped installation directory.
func RemoveLocal(dir string) error {
	lock, err := lockState(dir)
	if err != nil {
		return err
	}
	defer lock.Close()
	_, err = load(dir)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

func liveTLSConfig(dir string, initial tls.Certificate) *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		next, err := TLSCertificate(dir)
		if err != nil {
			return &initial, nil
		}
		return &next, nil
	}}
}
