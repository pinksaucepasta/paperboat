//go:build darwin || linux || windows

package availability

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostservice"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

const PolicySchemaV1 = "paperboat.availability-policy/v1"

var ErrInvalid = errors.New("invalid availability runtime contract")

type IdentitySource interface {
	Token(context.Context) (string, error)
}
type ProofSource interface {
	Proof(context.Context, string, string, string, []byte) ([]byte, error)
}

type Resolution struct {
	Schema        string `json:"schema"`
	UserMachineID string `json:"machine_id"`
	Mode          string `json:"mode"`
	Version       int64  `json:"version"`
}

type Observation struct {
	Schema             string    `json:"schema"`
	Mode               string    `json:"mode"`
	Version            int64     `json:"version"`
	Status             string    `json:"status"`
	ObservedAt         time.Time `json:"observed_at"`
	ErrorCode          string    `json:"error_code,omitempty"`
	HostServiceVersion string    `json:"host_service_version"`
	HostServiceScope   string    `json:"host_service_scope"`
	UpdateRollbacks    uint64    `json:"update_rollbacks"`
	UpdateHealth       string    `json:"update_health"`
}

type Resolver struct {
	endpoint    *url.URL
	identities  IdentitySource
	proofs      ProofSource
	operationID func() (string, error)
	client      *http.Client
}

func NewResolver(endpoint string, identities IdentitySource, proofs ProofSource, operationID func() (string, error), client *http.Client) (*Resolver, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Hostname() == "" || parsed.Path != "/v1/helper-runtime-policies/resolve" || parsed.RawQuery != "" || parsed.Fragment != "" || identities == nil || proofs == nil || operationID == nil || client == nil || client.Timeout <= 0 {
		return nil, ErrInvalid
	}
	return &Resolver{endpoint: parsed, identities: identities, proofs: proofs, operationID: operationID, client: client}, nil
}

func (r *Resolver) Resolve(ctx context.Context) (Resolution, error) {
	body := []byte("{}")
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return Resolution{}, controlFailure(err)
	}
	token, err := r.identities.Token(ctx)
	if err != nil {
		return Resolution{}, controlFailure(err)
	}
	operationID, err := r.operationID()
	if err != nil {
		return Resolution{}, controlFailure(err)
	}
	proof, err := r.proofs.Proof(ctx, operationID, http.MethodPost, r.endpoint.Path, body)
	if err != nil {
		return Resolution{}, controlFailure(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString(proof))
	request.Header.Set("Content-Type", "application/json")
	response, err := r.client.Do(request)
	if err != nil {
		return Resolution{}, controlFailure(err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return Resolution{}, controlFailure(errorreport.HTTPStatusFailure(response))
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 16<<10))
	decoder.DisallowUnknownFields()
	var envelope struct {
		Data Resolution `json:"data"`
	}
	var extra any
	if err := decoder.Decode(&envelope); err != nil {
		return Resolution{}, controlFailure(errors.Join(ErrInvalid, err))
	}
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return Resolution{}, controlFailure(errors.Join(ErrInvalid, err))
		}
		return Resolution{}, controlFailure(ErrInvalid)
	}
	if envelope.Data.Schema != PolicySchemaV1 || envelope.Data.UserMachineID == "" || !validMode(envelope.Data.Mode) || envelope.Data.Version < 0 {
		return Resolution{}, controlFailure(ErrInvalid)
	}
	return envelope.Data, nil
}

type HostClient struct {
	socketPath string
	timeout    time.Duration
}

func (c *HostClient) Apply(ctx context.Context, policy Resolution) (Observation, error) {
	if policy.Schema != PolicySchemaV1 || !validMode(policy.Mode) || policy.Version < 0 {
		return Observation{}, ErrInvalid
	}
	connection, err := dialAvailabilityHostService(ctx, c.socketPath, c.timeout)
	if err != nil {
		return Observation{}, localFailure(err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(c.timeout)); err != nil {
		return Observation{}, localFailure(err)
	}
	request := hostservice.Request{Schema: hostservice.ProtocolV1, Operation: "apply_availability", Mode: policy.Mode, Version: policy.Version}
	if err := json.NewEncoder(connection).Encode(request); err != nil {
		return Observation{}, localFailure(err)
	}
	if err := closeAvailabilityHostServiceWrite(connection); err != nil {
		return Observation{}, localFailure(err)
	}
	decoder := json.NewDecoder(io.LimitReader(connection, 16<<10))
	decoder.DisallowUnknownFields()
	var response hostservice.Response
	var extra any
	if err := decoder.Decode(&response); err != nil {
		return Observation{}, localFailure(errors.Join(ErrInvalid, err))
	}
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return Observation{}, localFailure(errors.Join(ErrInvalid, err))
		}
		return Observation{}, localFailure(ErrInvalid)
	}
	if response.Schema != hostservice.ProtocolV1 || response.DesiredMode != policy.Mode || response.DesiredVersion != policy.Version || response.ObservedMode != policy.Mode || response.ObservedVersion != policy.Version || response.ObservedAt.IsZero() || response.HostServiceVersion == "" || response.Scope != "system" || !validStatus(response.Status, response.ErrorCode) || !validUpdateHealth(response.UpdateHealth) {
		return Observation{}, localFailure(ErrInvalid)
	}
	return Observation{Schema: PolicySchemaV1, Mode: response.ObservedMode, Version: response.ObservedVersion, Status: response.Status, ObservedAt: response.ObservedAt.UTC(), ErrorCode: response.ErrorCode, HostServiceVersion: response.HostServiceVersion, HostServiceScope: response.Scope, UpdateRollbacks: response.UpdateRollbacks, UpdateHealth: response.UpdateHealth}, nil
}

type Service struct {
	resolver interface {
		Resolve(context.Context) (Resolution, error)
	}
	host interface {
		Apply(context.Context, Resolution) (Observation, error)
	}
	interval time.Duration
	mu       sync.RWMutex
	current  *Observation
	cancel   context.CancelFunc
	done     chan struct{}
	metrics  interface {
		Record(string, float64, map[string]string) error
	}
	lastRollbacks uint64
	lastFailure   *failureClass
}

func NewService(resolver interface {
	Resolve(context.Context) (Resolution, error)
}, host interface {
	Apply(context.Context, Resolution) (Observation, error)
}, interval time.Duration, metrics ...interface {
	Record(string, float64, map[string]string) error
}) (*Service, error) {
	if resolver == nil || host == nil || interval <= 0 || interval > time.Minute {
		return nil, ErrInvalid
	}
	if len(metrics) > 1 {
		return nil, ErrInvalid
	}
	service := &Service{resolver: resolver, host: host, interval: interval}
	if len(metrics) == 1 {
		service.metrics = metrics[0]
	}
	return service, nil
}

func (s *Service) Start(ctx context.Context) error {
	if ctx == nil {
		return ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	if s.cancel != nil {
		s.mu.Unlock()
		return ErrInvalid
	}
	// The daemon owns the background reconciliation lifetime. Retain the
	// startup reference and recorder values without retaining a short-lived
	// caller cancellation; Shutdown remains the explicit lifetime boundary.
	lifetimeCtx := context.WithoutCancel(ctx)
	if !supportref.Valid(supportref.FromContext(lifetimeCtx)) {
		lifetimeCtx = supportref.WithContext(lifetimeCtx, supportref.New())
	}
	runCtx, cancel := context.WithCancel(lifetimeCtx)
	s.cancel, s.done = cancel, make(chan struct{})
	done := s.done
	s.mu.Unlock()
	// Resolve and apply once before returning. This prevents the runtime's
	// first heartbeat from reporting a durable "pending" state merely because
	// the reconciliation goroutine has not received its first scheduler tick.
	// Failures remain retryable in the background and do not prevent startup.
	_ = s.applyOnce(runCtx)
	go s.run(runCtx, done)
	return nil
}

func (s *Service) run(ctx context.Context, done chan struct{}) {
	defer close(done)
	backoff := time.Second
	for {
		err := s.applyOnce(ctx)
		wait := backoff
		if err == nil {
			wait, backoff = s.interval, time.Second
		} else if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (s *Service) applyOnce(ctx context.Context) error {
	resolution, err := s.resolver.Resolve(ctx)
	if err != nil {
		failure := controlFailure(err)
		s.observeFailure(ctx, controlRequestStage, controlRequestCode, failure)
		return failure
	}
	observation, err := s.host.Apply(ctx, resolution)
	if err != nil {
		failure := localFailure(err)
		s.observeFailure(ctx, localGatewayStage, localGatewayCode, failure)
		return failure
	}
	var recovered bool
	s.mu.Lock()
	if observation.UpdateRollbacks > s.lastRollbacks && s.metrics != nil {
		_ = s.metrics.Record("paperboat_runtime_update_rollbacks_total", float64(observation.UpdateRollbacks-s.lastRollbacks), nil)
	}
	s.lastRollbacks = observation.UpdateRollbacks
	copy := observation
	s.current = &copy
	recovered = s.lastFailure != nil
	s.lastFailure = nil
	s.mu.Unlock()
	if recovered {
		errorreport.Current().Lifecycle(ctx, "service", "runtime_observation", "recovered", "success")
	}
	return nil
}

type failureClass struct {
	stage, code, cause string
	errno, status      int
}

func (s *Service) observeFailure(ctx context.Context, stage, code string, err error) {
	fault := errorreport.ProjectFault(ctx, "paperboat-daemon", "runtime_observation", stage, code, err)
	if isObservedCancellation(fault) {
		return
	}
	class := failureClass{stage: fault.Stage, code: fault.Code, cause: fault.Cause, errno: fault.Errno, status: fault.HTTPStatus}
	s.mu.Lock()
	if s.lastFailure != nil && *s.lastFailure == class {
		s.mu.Unlock()
		return
	}
	s.lastFailure = &class
	s.mu.Unlock()
	if !errorreport.HTTPAttemptObserved(err) {
		errorreport.Current().ObserveFailure(ctx, "paperboat-daemon", "runtime_observation", stage, code, err)
	}
}

func (s *Service) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	cancel, done := s.cancel, s.done
	s.cancel, s.done = nil, nil
	s.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) Observation() *Observation {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.current == nil {
		return nil
	}
	copy := *s.current
	return &copy
}

func validMode(value string) bool {
	return value == hostservice.AllowSleep || value == hostservice.KeepAwake
}
func validStatus(status, code string) bool {
	return status == "applied" && code == "" || (status == "unsupported" || status == "error") && code != ""
}
func validUpdateHealth(value string) bool {
	// Update reporting is independent from availability application. A host
	// service without updater diagnostics must still be able to acknowledge a
	// successfully applied power policy instead of leaving it pending forever.
	return value == "unknown" || value == "healthy" || value == "recovery_required"
}
