package enrollment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/internal/httptransport"
)

var (
	ErrInvalid                       = errors.New("invalid helper enrollment")
	ErrEnrollmentExchangeRejected    = errors.New("helper enrollment exchange was rejected")
	ErrEnrollmentExchangeUnavailable = errors.New("helper enrollment exchange is unavailable")
)

const enrollmentExchangeAttempts = 4

type Config struct {
	ControlURL           string `json:"control_url"`
	ControlCAFile        string `json:"control_ca_file,omitempty"`
	StateRoot            string `json:"state_root"`
	EnrollmentCredential string `json:"enrollment_credential"`
}

type RuntimeIdentity struct {
	Version       int       `json:"version"`
	HelperID      string    `json:"helper_id"`
	MachineID     string    `json:"machine_id"`
	EnvironmentID string    `json:"environment_id"`
	Credential    string    `json:"credential"`
	ExpiresAt     time.Time `json:"expires_at"`
	KeyID         string    `json:"key_id"`
}

type Client struct {
	transport http.RoundTripper
	timeout   time.Duration
}

type TokenSource struct {
	StateRoot string
	Clock     func() time.Time
}

type ProofSource struct {
	StateRoot string
	Clock     func() time.Time
}

func (s ProofSource) Proof(_ context.Context, operationID, method, path string, body []byte) ([]byte, error) {
	return s.proof(operationID, method, path, body, false)
}

func (s ProofSource) renewalProof(operationID, method, path string, body []byte) ([]byte, error) {
	return s.proof(operationID, method, path, body, true)
}

func (s ProofSource) proof(operationID, method, path string, body []byte, allowExpiredIdentity bool) ([]byte, error) {
	now := time.Now().UTC()
	if s.Clock != nil {
		now = s.Clock().UTC()
	}
	var value RuntimeIdentity
	var err error
	if allowExpiredIdentity {
		value, err = LoadRuntimeIdentityForRenewal(s.StateRoot, now)
	} else {
		value, err = LoadRuntimeIdentity(s.StateRoot, now)
	}
	if err != nil || len(operationID) < 8 || len(operationID) > 128 || method != http.MethodPost && method != http.MethodPut || path == "" || len(body) > 1<<20 {
		return nil, ErrInvalid
	}
	store, err := identity.Open(identity.Config{StateRoot: s.StateRoot})
	if err != nil || store.Current().ID != value.KeyID {
		return nil, ErrInvalid
	}
	bodyHash := sha256.Sum256(body)
	claims := struct {
		HelperID      string    `json:"helper_id"`
		EnvironmentID string    `json:"environment_id"`
		OperationID   string    `json:"operation_id"`
		Method        string    `json:"method"`
		Path          string    `json:"path"`
		BodySHA256    string    `json:"body_sha256"`
		IssuedAt      time.Time `json:"issued_at"`
		ExpiresAt     time.Time `json:"expires_at"`
	}{value.HelperID, value.EnvironmentID, operationID, method, path, base64.RawURLEncoding.EncodeToString(bodyHash[:]), now, now.Add(time.Minute)}
	payload, err := json.Marshal(claims)
	if err != nil {
		return nil, err
	}
	key := store.Current()
	return json.Marshal(struct {
		Algorithm string `json:"alg"`
		Payload   string `json:"payload"`
		Signature string `json:"signature"`
	}{"EdDSA", base64.RawURLEncoding.EncodeToString(payload), base64.RawURLEncoding.EncodeToString(key.Sign(payload))})
}

func (s TokenSource) Token(context.Context) (string, error) {
	now := time.Now().UTC()
	if s.Clock != nil {
		now = s.Clock().UTC()
	}
	value, err := LoadRuntimeIdentity(s.StateRoot, now)
	if err != nil {
		return "", err
	}
	return value.Credential, nil
}

func NewClient(transport http.RoundTripper, timeout time.Duration) (*Client, error) {
	if timeout <= 0 || timeout > 30*time.Second {
		return nil, ErrInvalid
	}
	return &Client{transport: transport, timeout: timeout}, nil
}

func LoadConfig(path string) (Config, error) {
	if !filepath.IsAbs(path) {
		return Config{}, ErrInvalid
	}
	info, err := os.Lstat(path)
	if err != nil || !secureEnrollmentConfigFile(path, info, 32<<10) {
		return Config{}, ErrInvalid
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var config Config
	if strictJSON(body, &config) != nil || !filepath.IsAbs(config.StateRoot) || config.ControlCAFile != "" && !filepath.IsAbs(config.ControlCAFile) || len(config.EnrollmentCredential) < 32 || len(config.EnrollmentCredential) > 16<<10 {
		return Config{}, ErrInvalid
	}
	return config, nil
}

func (c *Client) Enroll(ctx context.Context, config Config) (RuntimeIdentity, error) {
	if len(config.EnrollmentCredential) < 32 || len(config.EnrollmentCredential) > 16<<10 {
		return RuntimeIdentity{}, ErrInvalid
	}
	return c.enroll(ctx, config, "/v1/helper-enrollments", struct {
		Credential string `json:"credential"`
		PublicKey  string `json:"public_key"`
	}{Credential: config.EnrollmentCredential})
}

func (c *Client) enroll(ctx context.Context, config Config, endpointPath string, payload any) (RuntimeIdentity, error) {
	base, err := url.Parse(config.ControlURL)
	if err != nil || base.Scheme != "https" || base.User != nil || base.Hostname() == "" || base.RawQuery != "" || base.Fragment != "" {
		return RuntimeIdentity{}, ErrInvalid
	}
	base.Path = strings.TrimRight(base.Path, "/") + endpointPath
	store, err := identity.Open(identity.Config{StateRoot: config.StateRoot})
	if err != nil {
		return RuntimeIdentity{}, err
	}
	key := store.Current()
	switch value := payload.(type) {
	case struct {
		Credential string `json:"credential"`
		PublicKey  string `json:"public_key"`
	}:
		value.PublicKey = base64.RawURLEncoding.EncodeToString(key.Public())
		payload = value
	case struct {
		WorkloadIdentity string `json:"workload_identity"`
		PublicKey        string `json:"public_key"`
	}:
		value.PublicKey = base64.RawURLEncoding.EncodeToString(key.Public())
		payload = value
	default:
		return RuntimeIdentity{}, ErrInvalid
	}
	body, _ := json.Marshal(payload)
	transport := c.transport
	if transport == nil {
		transport, err = controlTransport(config.ControlCAFile)
		if err != nil {
			return RuntimeIdentity{}, err
		}
	}
	client := &http.Client{Transport: errorreport.TransportOperation(transport, base.String(), "helper_enrollment"), CheckRedirect: func(*http.Request, []*http.Request) error { return ErrInvalid }}
	var responseBody []byte
	for attempt := 0; attempt < enrollmentExchangeAttempts; attempt++ {
		status, value, exchangeErr := c.enrollmentExchangeAttempt(ctx, client, base.String(), body)
		if exchangeErr == nil && status >= 200 && status < 300 {
			responseBody = value
			break
		}
		if ctx.Err() != nil {
			return RuntimeIdentity{}, ctx.Err()
		}
		if exchangeErr != nil && errors.Is(exchangeErr, ErrInvalid) || exchangeErr == nil && !retryableEnrollmentExchangeStatus(status) {
			return RuntimeIdentity{}, errors.Join(ErrInvalid, ErrEnrollmentExchangeRejected)
		}
		if attempt+1 == enrollmentExchangeAttempts {
			return RuntimeIdentity{}, ErrEnrollmentExchangeUnavailable
		}
		delay := 500 * time.Millisecond << attempt
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return RuntimeIdentity{}, ctx.Err()
		case <-timer.C:
		}
	}
	var envelope struct {
		Data RuntimeIdentity `json:"data"`
	}
	if strictJSON(responseBody, &envelope) != nil {
		return RuntimeIdentity{}, ErrInvalid
	}
	result := envelope.Data
	result.Version = 1
	result.KeyID = key.ID
	if result.HelperID == "" || result.MachineID == "" || result.EnvironmentID == "" || len(result.Credential) < 32 || result.ExpiresAt.Before(time.Now().UTC()) {
		return RuntimeIdentity{}, ErrInvalid
	}
	if err := writeIdentity(config.StateRoot, result); err != nil {
		return RuntimeIdentity{}, err
	}
	return result, nil
}

func (c *Client) enrollmentExchangeAttempt(ctx context.Context, client *http.Client, endpoint string, body []byte) (int, []byte, error) {
	requestCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 64<<10+1))
	if err != nil {
		return response.StatusCode, nil, err
	}
	if len(responseBody) > 64<<10 {
		return response.StatusCode, nil, ErrInvalid
	}
	return response.StatusCode, responseBody, nil
}

func retryableEnrollmentExchangeStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooEarly || status == http.StatusTooManyRequests || status >= 500 && status <= 599
}

func controlTransport(caPath string) (http.RoundTripper, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13}
	if caPath != "" {
		info, err := os.Lstat(caPath)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 1 || info.Size() > 1<<20 {
			return nil, ErrInvalid
		}
		encoded, err := os.ReadFile(caPath)
		if err != nil {
			return nil, err
		}
		roots, err := x509.SystemCertPool()
		if err != nil || roots == nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(encoded) {
			return nil, ErrInvalid
		}
		tlsConfig.RootCAs = roots
	}
	transportConfig := httptransport.DevelopmentConfig()
	transportConfig.TLSConfig = tlsConfig
	return httptransport.New(transportConfig)
}

func writeIdentity(root string, value RuntimeIdentity) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return atomicfile.Write(filepath.Join(root, "runtime-identity.json"), encoded, atomicfile.Options{Mode: 0o600, OwnerUID: -1, OwnerGID: -1})
}

func LoadRuntimeIdentity(root string, now time.Time) (RuntimeIdentity, error) {
	return loadRuntimeIdentity(root, now, 0)
}

func LoadRuntimeIdentityForRenewal(root string, now time.Time) (RuntimeIdentity, error) {
	return loadRuntimeIdentity(root, now, -1)
}

func loadRuntimeIdentity(root string, now time.Time, expiryGrace time.Duration) (RuntimeIdentity, error) {
	if !filepath.IsAbs(root) || now.IsZero() {
		return RuntimeIdentity{}, ErrInvalid
	}
	path := filepath.Join(root, "runtime-identity.json")
	info, err := os.Lstat(path)
	if err != nil {
		return RuntimeIdentity{}, err
	}
	if !secureIdentityFile(path, info, 32<<10) {
		return RuntimeIdentity{}, ErrInvalid
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return RuntimeIdentity{}, err
	}
	var value RuntimeIdentity
	if strictJSON(body, &value) != nil {
		return RuntimeIdentity{}, ErrInvalid
	}
	validExpiry := expiryGrace < 0 || value.ExpiresAt.After(now.Add(-expiryGrace))
	if value.Version != 1 || value.HelperID == "" || value.MachineID == "" || value.EnvironmentID == "" || len(value.Credential) < 32 || !validExpiry || value.KeyID == "" {
		return RuntimeIdentity{}, ErrInvalid
	}
	store, err := identity.Open(identity.Config{StateRoot: root})
	if err != nil || store.Current().ID != value.KeyID {
		return RuntimeIdentity{}, ErrInvalid
	}
	return value, nil
}

func strictJSON(data []byte, target any) error {
	if err := rejectDuplicateJSON(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return ErrInvalid
	}
	return nil
}

func rejectDuplicateJSON(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, compound := token.(json.Delim)
		if !compound {
			return nil
		}
		switch delimiter {
		case '{':
			seen := make(map[string]struct{})
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return ErrInvalid
				}
				if _, exists := seen[key]; exists {
					return ErrInvalid
				}
				seen[key] = struct{}{}
				if err := walk(); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
		default:
			return ErrInvalid
		}
		_, err = decoder.Token()
		return err
	}
	if err := walk(); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return ErrInvalid
	}
	return nil
}
