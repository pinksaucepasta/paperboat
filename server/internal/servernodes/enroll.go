package servernodes

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/nacl/box"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/selfhost"
)

// Enrollment mirrors global edges (selfhost/enrollment.go): an unenrolled node prints a one-time claim code
// (its TLS SPKI pin and a secret); an operator gives the code to any node, which dials the new one pinned to
// that SPKI, checks its signed inspection, records it and delivers the runtime config sealed to the node's
// box key. Prior art: Teleport join tokens (~/ref/teleport/lib/auth/join/join.go Register).

const (
	codePrefix  = "pbs1"
	CodeLife    = 15 * time.Minute
	maxBundle   = 1 << 20
	inspectPath = "/v1/server-node/inspect"
	claimPath   = "/v1/server-node/claim"
)

var (
	ErrInvalid      = errors.New("server node enrollment request is invalid")
	ErrUnauthorized = errors.New("server node did not prove the claim code")
	ErrExpired      = errors.New("server node claim code expired")
	ErrUsed         = errors.New("a server node with this region and instance is already enrolled")
	ErrUnreachable  = errors.New("server node address is not reachable")

	regionPattern   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,23}$`)
	instancePattern = regexp.MustCompile(`^slot-(0[1-9]|[1-5][0-9]|6[0-4])$`)
	hostPattern     = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
)

// Inspection is what an unenrolled node states about itself, signed with its enrollment key.
type Inspection struct {
	Nonce         string `json:"nonce"`
	Region        string `json:"region"`
	Instance      string `json:"instance"`
	EndpointHost  string `json:"endpoint_host"`
	Version       string `json:"version"`
	Commit        string `json:"commit"`
	Certificate   string `json:"certificate"`
	EnrollmentKey string `json:"enrollment_key"`
	BoxKey        string `json:"box_key"`
	ExpiresAt     int64  `json:"expires_at"`
	Signature     string `json:"signature,omitempty"`
}

type claim struct {
	Nonce  string `json:"nonce"`
	Sealed []byte `json:"sealed"`
}

// Receipt proves the node opened the bundle: the hash is over the plaintext only the node could read.
type receipt struct {
	Nonce      string `json:"nonce"`
	BundleHash []byte `json:"bundle_hash"`
	Signature  string `json:"signature,omitempty"`
}

func sign(key ed25519.PrivateKey, v any) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, b))
}

func verify(pub []byte, sig string, v any) bool {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(sig)
	b, _ := json.Marshal(v)
	return err == nil && len(pub) == ed25519.PublicKeySize && ed25519.Verify(pub, b, raw)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func unb64(s string, n int) ([]byte, bool) {
	b, err := base64.RawURLEncoding.Strict().DecodeString(s)
	return b, err == nil && len(b) == n
}

func parseCode(code string) (pin []byte, secret string, err error) {
	parts := strings.Split(strings.TrimSpace(code), ".")
	if len(parts) != 3 || parts[0] != codePrefix {
		return nil, "", ErrInvalid
	}
	pin, ok := unb64(parts[1], 32)
	if _, ok2 := unb64(parts[2], 32); !ok || !ok2 {
		return nil, "", ErrInvalid
	}
	return pin, parts[2], nil
}

// Unenrolled is a node waiting to be claimed. It holds no database credentials.
type Unenrolled struct {
	Region, Instance, EndpointHost, Version, Commit string
	BundlePath                                      string // written 0600 once claimed

	tlsCert   tls.Certificate
	certPEM   string
	pin       [32]byte
	secret    string
	signKey   ed25519.PrivateKey
	boxPub    *[32]byte
	boxPriv   *[32]byte
	expiresAt time.Time
	mu        sync.Mutex
	nonce     string
	claimed   chan struct{}
}

// NewUnenrolled makes fresh TLS, enrollment and box keys and returns the node with its claim code.
func NewUnenrolled(region, instance, endpointHost, version, commit, bundlePath string) (*Unenrolled, string, error) {
	if !regionPattern.MatchString(region) || !instancePattern.MatchString(instance) || !validHost(endpointHost) || bundlePath == "" {
		return nil, "", ErrInvalid
	}
	u := &Unenrolled{Region: region, Instance: instance, EndpointHost: endpointHost, Version: version, Commit: commit,
		BundlePath: bundlePath, expiresAt: time.Now().Add(CodeLife), claimed: make(chan struct{})}
	tlsKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, "", err
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: endpointHost},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: u.expiresAt.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	if ip, perr := netip.ParseAddr(endpointHost); perr == nil {
		tmpl.IPAddresses = []net.IP{ip.AsSlice()}
	} else {
		tmpl.DNSNames = []string{endpointHost}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &tlsKey.PublicKey, tlsKey)
	if err != nil {
		return nil, "", err
	}
	cert, _ := x509.ParseCertificate(der)
	u.tlsCert = tls.Certificate{Certificate: [][]byte{der}, PrivateKey: tlsKey}
	u.certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	u.pin = sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	if _, u.signKey, err = ed25519.GenerateKey(rand.Reader); err != nil {
		return nil, "", err
	}
	if u.boxPub, u.boxPriv, err = box.GenerateKey(rand.Reader); err != nil {
		return nil, "", err
	}
	secret := make([]byte, 32)
	if _, err = rand.Read(secret); err != nil {
		return nil, "", err
	}
	u.secret = b64(secret)
	return u, codePrefix + "." + b64(u.pin[:]) + "." + u.secret, nil
}

// TLSConfig serves the pinned self-signed certificate.
func (u *Unenrolled) TLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{u.tlsCert}}
}

// Claimed is closed once the bundle is stored; the node then restarts with it as its config.
func (u *Unenrolled) Claimed() <-chan struct{} { return u.claimed }

func (u *Unenrolled) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if r.Method != http.MethodPost || subtle.ConstantTimeCompare([]byte(auth), []byte(u.secret)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	select {
	case <-u.claimed:
		http.Error(w, "already claimed", http.StatusConflict)
		return
	default:
	}
	if time.Now().After(u.expiresAt) {
		http.Error(w, "expired", http.StatusGone)
		return
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2*maxBundle))
	dec.DisallowUnknownFields()
	switch r.URL.Path {
	case inspectPath:
		var in struct {
			Nonce string `json:"nonce"`
		}
		if dec.Decode(&in) != nil || len(in.Nonce) < 32 || len(in.Nonce) > 128 {
			http.Error(w, "invalid", http.StatusBadRequest)
			return
		}
		u.nonce = in.Nonce
		out := Inspection{Nonce: in.Nonce, Region: u.Region, Instance: u.Instance, EndpointHost: u.EndpointHost, Version: u.Version,
			Commit: u.Commit, Certificate: u.certPEM, EnrollmentKey: b64(u.signKey.Public().(ed25519.PublicKey)),
			BoxKey: b64(u.boxPub[:]), ExpiresAt: u.expiresAt.Unix()}
		out.Signature = sign(u.signKey, out)
		_ = json.NewEncoder(w).Encode(out)
	case claimPath:
		var in claim
		if dec.Decode(&in) != nil || u.nonce == "" || in.Nonce != u.nonce {
			http.Error(w, "invalid", http.StatusBadRequest)
			return
		}
		bundle, ok := box.OpenAnonymous(nil, in.Sealed, u.boxPub, u.boxPriv)
		if !ok || len(bundle) > maxBundle || !json.Valid(bundle) {
			http.Error(w, "invalid", http.StatusBadRequest)
			return
		}
		if err := writePrivate(u.BundlePath, bundle); err != nil {
			http.Error(w, "store failed", http.StatusInternalServerError)
			return
		}
		sum := sha256.Sum256(bundle)
		out := receipt{Nonce: in.Nonce, BundleHash: sum[:]}
		out.Signature = sign(u.signKey, out)
		_ = json.NewEncoder(w).Encode(out)
		close(u.claimed)
	default:
		http.NotFound(w, r)
	}
}

// writePrivate stores the bundle owner-only, atomically, so a crash never leaves a half config.
func writePrivate(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Enroller is the operator side, run by any enrolled node.
type Enroller struct {
	DB     *db.DB
	Config config.Config // the enrolling node's own config: the base of the delivered bundle
	// Dial resolves the operator-given address to the dial address; nil refuses non-public addresses.
	Dial func(ctx context.Context, address string) (string, error)
}

// Enroll claims the node behind address with code and returns its recorded status.
func (e *Enroller) Enroll(ctx context.Context, actor, address, code string) (Status, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	pin, secret, err := parseCode(code)
	if err != nil {
		return Status{}, err
	}
	host, _, splitErr := net.SplitHostPort(address)
	if splitErr != nil {
		host, address = address, net.JoinHostPort(address, "8443")
	}
	dial := e.Dial
	if dial == nil {
		dial = publicDialAddress
	}
	dialAddress, err := dial(ctx, address)
	if err != nil {
		return Status{}, err
	}
	transport := selfhost.PinnedTransport(pin, dialAddress)
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	target := "https://" + address
	nonce := make([]byte, 32)
	_, _ = rand.Read(nonce)
	var in Inspection
	if err := post(ctx, client, target+inspectPath, secret, map[string]string{"nonce": b64(nonce)}, &in); err != nil {
		return Status{}, err
	}
	signKey, boxKey, err := checkInspection(in, b64(nonce), host, pin)
	if err != nil {
		return Status{}, err
	}
	bundle, err := e.bundle(in)
	if err != nil {
		return Status{}, err
	}
	id := in.Region + "-" + in.Instance
	tag, err := e.DB.Pool().Exec(ctx, `INSERT INTO paperboat.control_server_nodes
  (id, region, state, version, commit, started_at, last_seen_at, endpoint_host, tls_spki_sha256, enrolled_by)
VALUES ($1,$2,'registered',$3,$4,now(),now(),$5,$6,$7)
ON CONFLICT (id) DO UPDATE SET state='registered', version=excluded.version, commit=excluded.commit, started_at=now(),
  last_seen_at=now(), endpoint_host=excluded.endpoint_host, tls_spki_sha256=excluded.tls_spki_sha256,
  enrolled_by=excluded.enrolled_by
WHERE control_server_nodes.state='removed'`,
		id, in.Region, in.Version, in.Commit, in.EndpointHost, pin, actor)
	if err != nil {
		return Status{}, err
	}
	if tag.RowsAffected() == 0 {
		return Status{}, ErrUsed
	}
	sealed, err := box.SealAnonymous(nil, bundle, boxKey, rand.Reader)
	if err != nil {
		return Status{}, e.forget(id, err)
	}
	var got receipt
	if err := post(ctx, client, target+claimPath, secret, claim{Nonce: in.Nonce, Sealed: sealed}, &got); err != nil {
		return Status{}, e.forget(id, err)
	}
	sum := sha256.Sum256(bundle)
	sig := got.Signature
	got.Signature = ""
	if got.Nonce != in.Nonce || subtle.ConstantTimeCompare(got.BundleHash, sum[:]) != 1 || !verify(signKey, sig, got) {
		return Status{}, e.forget(id, ErrUnauthorized)
	}
	return Status{ID: id, Region: in.Region, State: "registered", Version: in.Version, Commit: in.Commit}, nil
}

// forget marks a registration whose delivery failed removed, so the operator can retry with a fresh code and
// the region's DNS is still reconciled.
func (e *Enroller) forget(id string, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := e.DB.Pool().Exec(ctx, `UPDATE paperboat.control_server_nodes SET state='removed' WHERE id=$1 AND state='registered'`, id); err != nil {
		return errors.Join(cause, fmt.Errorf("remove the unfinished registration %s: %w", id, err))
	}
	return cause
}

func checkInspection(in Inspection, nonce, host string, pin []byte) (signKey []byte, boxKey *[32]byte, err error) {
	sig := in.Signature
	in.Signature = ""
	signKey, ok := unb64(in.EnrollmentKey, ed25519.PublicKeySize)
	boxRaw, ok2 := unb64(in.BoxKey, 32)
	if !ok || !ok2 || !verify(signKey, sig, in) {
		return nil, nil, ErrUnauthorized
	}
	if in.Nonce != nonce || in.EndpointHost != host || !regionPattern.MatchString(in.Region) || !instancePattern.MatchString(in.Instance) ||
		in.Version == "" || len(in.Version) > 64 || in.Commit == "" || len(in.Commit) > 64 {
		return nil, nil, ErrInvalid
	}
	if time.Now().Unix() > in.ExpiresAt {
		return nil, nil, ErrExpired
	}
	block, _ := pem.Decode([]byte(in.Certificate))
	if block == nil {
		return nil, nil, ErrInvalid
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, ErrInvalid
	}
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	if subtle.ConstantTimeCompare(sum[:], pin) != 1 || cert.VerifyHostname(host) != nil {
		return nil, nil, ErrUnauthorized
	}
	boxKey = new([32]byte)
	copy(boxKey[:], boxRaw)
	return signKey, boxKey, nil
}

// bundle is the enrolling node's config with the new node's own identity.
func (e *Enroller) bundle(in Inspection) ([]byte, error) {
	cfg := e.Config
	cfg.Reporting.Region, cfg.Reporting.Instance = in.Region, in.Instance
	return json.Marshal(cfg)
}

func post(ctx context.Context, c *http.Client, target, secret string, in, out any) error {
	body, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return ErrInvalid
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		if errors.Is(err, selfhost.ErrUnauthorized) {
			return ErrUnauthorized
		}
		return fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return ErrUnauthorized
	case http.StatusGone:
		return ErrExpired
	case http.StatusConflict:
		return ErrUsed
	default:
		return fmt.Errorf("%w: status %d", ErrUnreachable, resp.StatusCode)
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, maxBundle))
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	return nil
}

func validHost(h string) bool {
	if _, err := netip.ParseAddr(h); err == nil {
		return true
	}
	return len(h) <= 253 && hostPattern.MatchString(h)
}

// publicDialAddress refuses loopback, private and link-local targets, so the enrollment API cannot reach
// internal services.
func publicDialAddress(ctx context.Context, address string) (string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", ErrInvalid
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return "", ErrUnreachable
	}
	for _, ip := range ips {
		ip = ip.Unmap()
		if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			return "", ErrInvalid
		}
	}
	return net.JoinHostPort(ips[0].String(), port), nil
}
