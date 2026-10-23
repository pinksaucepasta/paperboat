// Package selfhost owns account/team/global-scoped installation authority.
package selfhost

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/nodeauth"
	"github.com/pinksaucepasta/paperboat-server/internal/secrets"
	"net/netip"
	"strings"
	"sync"
	"time"
	"unicode"
)

var (
	ErrInvalid      = errors.New("invalid self-hosted server request")
	ErrUnauthorized = errors.New("self-hosted server authority rejected")
	ErrExpired      = errors.New("installation code expired; run pbh selfhost code again")
	ErrUsed         = errors.New("installation already belongs to another scope")
	ErrRevoked      = errors.New("installation removed; reinstall before enrollment")
	ErrLimit        = errors.New("self-hosted server limit reached")
	ErrUnreachable  = errors.New("server could not be verified at this address; check address, port and enrollment code")
)

type Service struct {
	ingressMu               sync.Mutex
	ingressCursor           string
	ingressObservationMu    sync.Mutex
	ingressFailureClass     string
	ingressFailureReference string
	database                *db.DB
	key                     string
	now                     func() time.Time
	ControlURL              string
	TrustDocuments          func(context.Context) ([]byte, []byte, error)
	DialAddress             func(context.Context, string) (string, error)
	// ProbeIngress replaces VerifyIngress inside ReconcileIngress when set (tests hold probes open).
	ProbeIngress            func(context.Context, string) error
}

func New(database *db.DB, encryptionKey string) *Service {
	return &Service{database: database, key: encryptionKey, now: time.Now}
}
func (s *Service) available() bool {
	return s != nil && s.database != nil && s.database.SQL() != nil && len(s.key) >= 32
}
func randomToken(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
func randomIdentity() string { return uuid.NewString() }
func validRegion(v string) bool {
	if len(v) == 0 || len(v) > 63 || v[0] < 'a' || v[0] > 'z' {
		return false
	}
	for _, c := range v {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}
func digest(v string) []byte { h := sha256.Sum256([]byte(v)); return h[:] }
func publicKey(v string) (ed25519.PublicKey, error) {
	b, e := base64.RawURLEncoding.Strict().DecodeString(v)
	if e != nil || len(b) != ed25519.PublicKeySize {
		return nil, ErrInvalid
	}
	return ed25519.PublicKey(b), nil
}

func validLabel(v string, max int) bool {
	return len(v) > 0 && len(v) <= max && strings.TrimSpace(v) == v && strings.IndexFunc(v, func(r rune) bool { return unicode.IsControl(r) || unicode.In(r, unicode.Cf) }) < 0
}
func validHost(v string) bool {
	if !validLabel(v, 253) {
		return false
	}
	if ip, e := netip.ParseAddr(v); e == nil {
		return publicIngressIP(ip)
	}
	if strings.ContainsAny(v, " /:@?#\\") {
		return false
	}
	for _, part := range strings.Split(v, ".") {
		if len(part) == 0 || len(part) > 63 || part[0] == '-' || part[len(part)-1] == '-' {
			return false
		}
		for _, c := range part {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return strings.Contains(v, ".")
}
func (s *Service) RuntimeNode(ctx context.Context, credential string) (string, error) {
	if !s.available() {
		return "", ErrUnauthorized
	}
	if len(credential) < 32 {
		return "", nodeauth.ErrNotFound
	}
	var id string
	e := s.database.SQL().QueryRowContext(ctx, `SELECT node_id FROM paperboat.selfhost_installations WHERE runtime_hash=$1 AND state='active' AND claim_complete AND (expires_at IS NULL OR expires_at>now())`, digest(credential)).Scan(&id)
	if e == sql.ErrNoRows {
		e = nodeauth.ErrNotFound
	}
	return id, e
}
func (s *Service) RuntimeCredential(ctx context.Context, nodeID string) (string, error) {
	if !s.available() {
		return "", ErrUnauthorized
	}
	var encrypted []byte
	e := s.database.SQL().QueryRowContext(ctx, `SELECT runtime_ciphertext FROM paperboat.selfhost_installations WHERE node_id=$1 AND state='active' AND claim_complete AND (expires_at IS NULL OR expires_at>now())`, nodeID).Scan(&encrypted)
	if e == sql.ErrNoRows {
		return "", nodeauth.ErrNotFound
	}
	if e != nil {
		return "", e
	}
	return secrets.Decrypt(s.key, encrypted)
}
func (s *Service) IsSelfHosted(ctx context.Context, nodeID string) (bool, error) {
	var yes bool
	if !s.available() {
		return false, ErrUnauthorized
	}
	e := s.database.SQL().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.selfhost_installations WHERE node_id=$1)`, nodeID).Scan(&yes)
	return yes, e
}
