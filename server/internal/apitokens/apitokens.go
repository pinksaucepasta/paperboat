// Package apitokens owns workspace API tokens (PLAN F18, shared/policy/README.md "API
// tokens"): a token acts as the member who created it, in one workspace, until it expires or
// is revoked, optionally narrowed by a scope. Only sha256(secret) is stored.
package apitokens

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
	"github.com/pinksaucepasta/paperboat/shared/policy"
)

// SecretPrefix starts every token secret: "pbat_" + 32 random bytes in base64url.
const SecretPrefix = "pbat_"

const secretBytes = 32

// lastUsedInterval bounds last_used_at writes to one per token per minute.
const lastUsedInterval = time.Minute

var (
	// ErrInvalid is an unknown, malformed, expired or revoked secret, or a creator who is no
	// longer active.
	ErrInvalid         = errors.New("api token invalid")
	ErrInvalidName     = errors.New("invalid api token name")
	ErrInvalidLifetime = errors.New("invalid api token lifetime")
	ErrNameTaken       = errors.New("api token name taken")
	ErrLimit           = errors.New("api token limit reached")
	ErrNotFound        = errors.New("api token not found")
)

// ScopeError is a scope that failed validation; Errors uses the policy error codes.
type ScopeError struct{ Errors policy.Errors }

func (e *ScopeError) Error() string { return "invalid api token scope: " + e.Errors.Error() }

// Token is one stored token as the API shows it. CreatedBy is "user:<email>".
type Token struct {
	ID         string
	AccountID  string
	Workspace  workspace.Scope
	Name       string
	CreatedBy  string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
	Scope      []policy.ScopeEntry
}

// Principal is an authenticated token: requests act as User in Workspace only. Scope nil
// means the token is not narrowed.
type Principal struct {
	TokenID   string
	User      auth.User
	Workspace workspace.Scope
	Scope     []policy.ScopeEntry
}

type CreateInput struct {
	Name string
	// ExpiresInDays 0 takes the configured default.
	ExpiresInDays int
	Scope         json.RawMessage
}

type Service struct {
	db     *db.DB
	audit  *audit.Writer
	limits config.APITokens
	// key encrypts a created token's secret for its Idempotency-Key retry window.
	key string
	now func() time.Time
}

func New(store *db.DB, writer *audit.Writer, limits config.APITokens, encryptionKey string) *Service {
	return &Service{db: store, audit: writer, limits: limits, key: encryptionKey, now: time.Now}
}

func (s *Service) lifetime(days int) (time.Duration, error) {
	if days == 0 {
		days = s.limits.DefaultLifetimeDays
	}
	if days < 1 || days > s.limits.MaxLifetimeDays {
		return 0, ErrInvalidLifetime
	}
	return time.Duration(days) * 24 * time.Hour, nil
}

// MaxLifetimeDays is the configured upper bound for expires_in_days.
func (s *Service) MaxLifetimeDays() int { return s.limits.MaxLifetimeDays }

// newSecret returns a fresh secret and its stored hash.
func newSecret() (string, []byte, error) {
	b := make([]byte, secretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	secret := SecretPrefix + base64.RawURLEncoding.EncodeToString(b)
	return secret, hashSecret(secret), nil
}

func hashSecret(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return sum[:]
}

// wellFormed reports whether secret has the exact shape of an issued secret.
func wellFormed(secret string) bool {
	body, ok := strings.CutPrefix(secret, SecretPrefix)
	if !ok || len(body) != base64.RawURLEncoding.EncodedLen(secretBytes) {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(body)
	return err == nil
}

func newID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "apitok_" + hex.EncodeToString(b), nil
}

type contextKey struct{}

// WithPrincipal marks ctx as authenticated by the token.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, contextKey{}, p)
}

// FromContext returns the token principal of a request authenticated by an API token.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(contextKey{}).(Principal)
	return p, ok
}
