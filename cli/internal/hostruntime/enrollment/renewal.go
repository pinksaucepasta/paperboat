package enrollment

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
)

type RenewingTokenConfig struct {
	ControlURL  string
	StateRoot   string
	Transport   http.RoundTripper
	RenewBefore time.Duration
	Timeout     time.Duration
	Clock       func() time.Time
	OperationID func() (string, error)
	Metrics     interface {
		Record(string, float64, map[string]string) error
	}
}

type RenewingTokenSource struct {
	config   RenewingTokenConfig
	endpoint *url.URL
	client   *http.Client
	mu       sync.Mutex
}

func NewRenewingTokenSource(config RenewingTokenConfig) (*RenewingTokenSource, error) {
	base, err := url.Parse(config.ControlURL)
	if err != nil {
		return nil, enrollmentFailure{classification: ErrInvalid, cause: err}
	}
	if base.Scheme != "https" || base.User != nil || base.Hostname() == "" || base.RawQuery != "" || base.Fragment != "" || config.StateRoot == "" || config.Transport == nil || config.OperationID == nil {
		return nil, ErrInvalid
	}
	if config.RenewBefore == 0 {
		config.RenewBefore = 10 * time.Minute
	}
	if config.Timeout == 0 {
		config.Timeout = 15 * time.Second
	}
	if config.Clock == nil {
		config.Clock = func() time.Time { return time.Now().UTC() }
	}
	if config.RenewBefore <= 0 || config.RenewBefore >= time.Hour || config.Timeout <= 0 || config.Timeout > 30*time.Second {
		return nil, ErrInvalid
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/v1/helper-identity-renewals"
	return &RenewingTokenSource{config: config, endpoint: base, client: &http.Client{Transport: errorreport.TransportOperation(config.Transport, base.String(), "identity_renewal"), CheckRedirect: func(*http.Request, []*http.Request) error { return errEnrollmentRedirect }}}, nil
}

func (s *RenewingTokenSource) Token(ctx context.Context) (token string, resultErr error) {
	defer func() {
		if resultErr != nil && s.config.Metrics != nil {
			if metricErr := s.config.Metrics.Record("paperboat_runtime_renewal_failures_total", 1, nil); metricErr != nil {
				errorreport.Current().ObserveFailure(ctx, "paperboat-daemon", "identity_renewal", "diagnostic_storage", "diagnostic_storage_unavailable", metricErr)
			}
		}
	}()
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.config.Clock().UTC()
	current, err := LoadRuntimeIdentityForRenewal(s.config.StateRoot, now)
	if err != nil {
		return "", unavailableEnrollment(err)
	}
	if current.ExpiresAt.After(now.Add(s.config.RenewBefore)) {
		return current.Credential, nil
	}
	operationID, err := s.config.OperationID()
	if err != nil {
		return "", unavailableEnrollment(err)
	}
	if len(operationID) < 8 || len(operationID) > 128 {
		return "", ErrInvalid
	}
	body, _ := json.Marshal(struct {
		OperationID string `json:"operation_id"`
	}{operationID})
	proof, err := (ProofSource{StateRoot: s.config.StateRoot, Clock: s.config.Clock}).renewalProof(operationID, http.MethodPost, s.endpoint.Path, body)
	if err != nil {
		return "", unavailableEnrollment(err)
	}
	requestCtx, cancel := context.WithTimeout(ctx, s.config.Timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, s.endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return "", unavailableEnrollment(err)
	}
	request.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString(proof))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := s.client.Do(request)
	if err != nil {
		return "", unavailableEnrollment(err)
	}
	encoded, err := readEnrollmentResponse(response)
	if err != nil {
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			err = errors.Join(errorreport.HTTPStatusFailure(response), err)
		}
		return "", unavailableEnrollment(err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			return "", ErrInvalid
		}
		return "", unavailableEnrollment(errorreport.HTTPStatusFailure(response))
	}
	var envelope struct {
		Data RuntimeIdentity `json:"data"`
	}
	if err := strictJSON(encoded, &envelope); err != nil {
		return "", unavailableEnrollment(err)
	}
	renewed := envelope.Data
	renewed.Version = 1
	renewed.KeyID = current.KeyID
	if renewed.HelperID != current.HelperID || renewed.EnvironmentID != current.EnvironmentID || len(renewed.Credential) < 32 || !renewed.ExpiresAt.After(current.ExpiresAt) {
		return "", ErrInvalid
	}
	if err := writeIdentity(s.config.StateRoot, renewed); err != nil {
		return "", unavailableEnrollment(err)
	}
	return renewed.Credential, nil
}

var _ interface {
	Token(context.Context) (string, error)
} = (*RenewingTokenSource)(nil)
