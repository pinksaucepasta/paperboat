package auth

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"

	"github.com/pinksaucepasta/paperboat/internal/httptransport"
	"sync"
	"time"
)

var (
	ErrJWKSInvalid   = errors.New("invalid JWKS")
	ErrJWKSOverLimit = errors.New("JWKS limit exceeded")
)

const (
	DefaultMaxJWKSBytes = 64 << 10
	// The longest frozen credential lifetime is one hour and verifier skew is
	// one minute. Deployments may lower this but must not undercut issued tokens.
	DefaultRetainMissing = 61 * time.Minute
)

// JWKSFetcher fetches the key set. maxAge is how long the server says the document stays current
// (Cache-Control max-age), zero when it does not say.
type JWKSFetcher interface {
	Fetch(context.Context) (body io.ReadCloser, maxAge time.Duration, err error)
}

// The server's max-age is honoured within these bounds: keys are re-read at least hourly (a withdrawn
// key otherwise stays usable longer) and never more than once a minute.
const (
	minimumJWKSMaxAge = time.Minute
	maximumJWKSMaxAge = time.Hour
)

// cacheMaxAge reads max-age from a Cache-Control header; zero when absent or invalid.
func cacheMaxAge(header string) time.Duration {
	for _, directive := range strings.Split(header, ",") {
		name, value, ok := strings.Cut(strings.TrimSpace(directive), "=")
		if !ok || !strings.EqualFold(name, "max-age") {
			continue
		}
		seconds, err := strconv.ParseInt(value, 10, 64)
		if err != nil || seconds <= 0 {
			return 0
		}
		return min(max(time.Duration(seconds)*time.Second, minimumJWKSMaxAge), maximumJWKSMaxAge)
	}
	return 0
}

type HTTPJWKSFetcher struct {
	endpoint *url.URL
	client   *http.Client
}

func NewHTTPJWKSFetcher(endpoint string, allowedHosts []string, transport http.RoundTripper) (*HTTPJWKSFetcher, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Hostname() == "" || parsed.Fragment != "" {
		return nil, ErrJWKSInvalid
	}
	allowed := false
	for _, host := range allowedHosts {
		if strings.EqualFold(strings.TrimSuffix(host, "."), strings.TrimSuffix(parsed.Hostname(), ".")) {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, ErrJWKSInvalid
	}
	if transport == nil {
		transport = httptransport.Default()
	}
	client := &http.Client{Transport: errorreport.TransportOperation(transport, parsed.String(), "authorization_keys"), CheckRedirect: func(*http.Request, []*http.Request) error { return ErrJWKSInvalid }}
	return &HTTPJWKSFetcher{endpoint: parsed, client: client}, nil
}

func (f *HTTPJWKSFetcher) Fetch(ctx context.Context) (io.ReadCloser, time.Duration, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, f.endpoint.String(), nil)
	if err != nil {
		return nil, 0, err
	}
	request.Header.Set("Accept", "application/json")
	response, err := f.client.Do(request)
	if err != nil {
		return nil, 0, err
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, 0, ErrJWKSInvalid
	}
	return response.Body, cacheMaxAge(response.Header.Get("Cache-Control")), nil
}

type JWKSConfig struct {
	Fetcher         JWKSFetcher
	Clock           Clock
	TTL             time.Duration
	RetainMissing   time.Duration
	MaxBytes        int64
	MaxKeys         int
	MaxRetainedKeys int
	PersistencePath string
}

type JWKSCache struct {
	mu        sync.RWMutex
	refreshMu sync.Mutex
	config    JWKSConfig
	keys      map[string]cachedKey
	// lastFetch and lastErr are guarded by refreshMu. Callers that arrive
	// within jwksRefreshCooldown of a fetch share its result: concurrent misses
	// collapse into one fetch and unknown key IDs cannot drive a fetch storm.
	lastFetch time.Time
	lastErr   error
	// ttl is how long the last fetched document stays current: the server's max-age, else config.TTL.
	ttl time.Duration
}

const jwksRefreshCooldown = 30 * time.Second

type cachedKey struct {
	key       ed25519.PublicKey
	expiresAt time.Time
	retired   bool
}

func NewJWKSCache(config JWKSConfig) (*JWKSCache, error) {
	if config.TTL == 0 {
		config.TTL = 5 * time.Minute
	}
	if config.RetainMissing == 0 {
		config.RetainMissing = DefaultRetainMissing
	}
	if config.MaxBytes == 0 {
		config.MaxBytes = DefaultMaxJWKSBytes
	}
	if config.MaxKeys == 0 {
		config.MaxKeys = 64
	}
	if config.MaxRetainedKeys == 0 {
		config.MaxRetainedKeys = 128
	}
	if config.Fetcher == nil || config.Clock == nil || config.TTL <= 0 || config.RetainMissing < config.TTL || config.MaxBytes < 1 || config.MaxBytes > DefaultMaxJWKSBytes || config.MaxKeys < 1 || config.MaxKeys > 64 || config.MaxRetainedKeys < config.MaxKeys || config.MaxRetainedKeys > 256 {
		return nil, ErrJWKSInvalid
	}
	cache := &JWKSCache{config: config, keys: make(map[string]cachedKey)}
	cache.loadPersisted()
	return cache, nil
}

func (c *JWKSCache) Lookup(_ context.Context, keyID string) (ed25519.PublicKey, bool, error) {
	if keyID == "" {
		return nil, false, nil
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	entry, ok := c.keys[keyID]
	if !ok || !c.config.Clock.Now().Before(entry.expiresAt) {
		return nil, false, nil
	}
	return append(ed25519.PublicKey(nil), entry.key...), true, nil
}

func (c *JWKSCache) Refresh(ctx context.Context) error {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	now := c.config.Clock.Now()
	if !c.lastFetch.IsZero() && now.Sub(c.lastFetch) < jwksRefreshCooldown {
		return c.lastErr
	}
	c.lastErr = c.fetch(ctx)
	c.lastFetch = now
	return c.lastErr
}

// RefreshInterval is when a periodic refresh should run again: half the current document's lifetime,
// so a key rotation is seen before the previous document ends.
func (c *JWKSCache) RefreshInterval() time.Duration {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	if c.ttl == 0 {
		return c.config.TTL / 2
	}
	return c.ttl / 2
}

func (c *JWKSCache) fetch(ctx context.Context) error {
	body, maxAge, err := c.config.Fetcher.Fetch(ctx)
	if err != nil {
		return err
	}
	ttl := c.config.TTL
	if maxAge > 0 {
		ttl = maxAge
	}
	if body == nil {
		return ErrJWKSInvalid
	}
	defer body.Close()
	limited := io.LimitReader(body, c.config.MaxBytes+1)
	encoded, err := io.ReadAll(limited)
	if err != nil {
		return err
	}
	if int64(len(encoded)) > c.config.MaxBytes {
		return ErrJWKSOverLimit
	}
	refreshed, withdrawn, err := decodeJWKS(encoded, c.config.MaxKeys)
	if err != nil {
		return err
	}
	c.ttl = ttl
	c.mu.Lock()
	now := c.config.Clock.Now()
	if err := c.persist(encoded, now); err != nil {
		c.mu.Unlock()
		return err
	}
	keys := make(map[string]cachedKey, len(refreshed)+len(c.keys))
	for keyID, key := range refreshed {
		keys[keyID] = cachedKey{key: key, expiresAt: now.Add(ttl)}
	}
	for keyID, previous := range c.keys {
		// A withdrawn key ends now; a key that only left the set is retained for the credentials it signed.
		if _, current := refreshed[keyID]; current || withdrawn[keyID] {
			continue
		}
		if previous.retired {
			if now.Before(previous.expiresAt) {
				keys[keyID] = previous
			}
		} else {
			keys[keyID] = cachedKey{key: previous.key, expiresAt: now.Add(c.config.RetainMissing), retired: true}
		}
	}
	if len(keys) > c.config.MaxRetainedKeys {
		c.mu.Unlock()
		return ErrJWKSOverLimit
	}
	c.keys = keys
	c.mu.Unlock()
	return nil
}

type persistedJWKS struct {
	Schema      string          `json:"schema"`
	ValidatedAt time.Time       `json:"validated_at"`
	Document    json.RawMessage `json:"document"`
}

func (c *JWKSCache) loadPersisted() {
	path := c.config.PersistencePath
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return
	}
	info, err := os.Lstat(path)
	if err != nil || !secureJWKSFile(path, info) || info.Size() < 1 || info.Size() > c.config.MaxBytes+4096 {
		return
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var persisted persistedJWKS
	var extra any
	if decoder.Decode(&persisted) != nil || decoder.Decode(&extra) != io.EOF || persisted.Schema != "paperboat.jwks-cache/v1" || persisted.ValidatedAt.IsZero() {
		return
	}
	now := c.config.Clock.Now()
	if persisted.ValidatedAt.After(now.Add(time.Minute)) || !now.Before(persisted.ValidatedAt.Add(c.config.TTL)) {
		return
	}
	keys, _, err := decodeJWKS(persisted.Document, c.config.MaxKeys)
	if err != nil {
		return
	}
	expiresAt := persisted.ValidatedAt.Add(c.config.TTL)
	for keyID, key := range keys {
		c.keys[keyID] = cachedKey{key: key, expiresAt: expiresAt}
	}
}

func (c *JWKSCache) persist(document []byte, validatedAt time.Time) error {
	path := c.config.PersistencePath
	if path == "" {
		return nil
	}
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return ErrJWKSInvalid
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrJWKSInvalid
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return err
	}
	body, err := json.Marshal(persistedJWKS{Schema: "paperboat.jwks-cache/v1", ValidatedAt: validatedAt.UTC(), Document: append(json.RawMessage(nil), document...)})
	if err != nil {
		return err
	}
	return atomicfile.Write(path, body, atomicfile.CurrentOwnerOptions(0o600))
}

type jwksDocument struct {
	Keys []jwk `json:"keys"`
	// WithdrawnKeyIDs names keys that must stop verifying immediately (omitted while empty).
	WithdrawnKeyIDs []string `json:"withdrawn_key_ids"`
}
type jwk struct {
	KeyType   string `json:"kty"`
	Curve     string `json:"crv"`
	Use       string `json:"use"`
	Algorithm string `json:"alg"`
	KeyID     string `json:"kid"`
	X         string `json:"x"`
}

func decodeJWKS(encoded []byte, maxKeys int) (map[string]ed25519.PublicKey, map[string]bool, error) {
	if err := rejectDuplicateKeys(encoded); err != nil {
		return nil, nil, ErrJWKSInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	var document jwksDocument
	if err := decoder.Decode(&document); err != nil {
		return nil, nil, ErrJWKSInvalid
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, nil, ErrJWKSInvalid
	}
	if len(document.Keys) == 0 || len(document.Keys) > maxKeys {
		return nil, nil, ErrJWKSOverLimit
	}
	keys := make(map[string]ed25519.PublicKey, len(document.Keys))
	for _, item := range document.Keys {
		if item.KeyType != "OKP" || item.Curve != "Ed25519" || item.Use != "sig" || item.Algorithm != "EdDSA" || len(item.KeyID) < 1 || len(item.KeyID) > 128 {
			return nil, nil, ErrJWKSInvalid
		}
		if _, duplicate := keys[item.KeyID]; duplicate {
			return nil, nil, ErrJWKSInvalid
		}
		x, err := base64.RawURLEncoding.DecodeString(item.X)
		if err != nil || len(x) != ed25519.PublicKeySize {
			return nil, nil, ErrJWKSInvalid
		}
		keys[item.KeyID] = append(ed25519.PublicKey(nil), x...)
	}
	if len(document.WithdrawnKeyIDs) > maxKeys {
		return nil, nil, ErrJWKSOverLimit
	}
	withdrawn := make(map[string]bool, len(document.WithdrawnKeyIDs))
	for _, keyID := range document.WithdrawnKeyIDs {
		if len(keyID) < 1 || len(keyID) > 128 || keys[keyID] != nil {
			return nil, nil, ErrJWKSInvalid
		}
		withdrawn[keyID] = true
	}
	return keys, withdrawn, nil
}
