// Package configsync owns the helper-side configuration synchronization
// runtime and its authenticated control-plane clients.
package configsync

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/httptransport"
)

var (
	ErrControlClientInvalid = errors.New("invalid config sync control client")
	ErrAuthorization        = errors.New("config sync authorization is unavailable")
	ErrLeaseBusy            = errors.New("config repository lease is busy")
	ErrLeaseLost            = errors.New("config repository lease is lost")
	ErrOperationConflict    = errors.New("config sync operation conflicts with an earlier request")
	ErrWritesDisabled       = errors.New("config repository writes are disabled")
)

const maxControlResponseBytes = 64 << 10

type TokenSource interface {
	Token(context.Context) (string, error)
}

type ProofSource interface {
	Proof(context.Context, string, string, string, []byte) ([]byte, error)
}

type OperationIDSource func() (string, error)

type ControlClientConfig struct {
	BaseURL         string
	AllowedHosts    []string
	RepositoryHosts []string
	Identities      TokenSource
	Proofs          ProofSource
	OperationID     OperationIDSource
	Transport       http.RoundTripper
	Timeout         time.Duration
	Clock           func() time.Time
}

type ControlClient struct {
	base            *url.URL
	identities      TokenSource
	proofs          ProofSource
	operationID     OperationIDSource
	client          *http.Client
	clock           func() time.Time
	repositoryHosts map[string]bool

	mu         sync.Mutex
	credential Credential
	access     RepositoryAccess
	accesses   map[string]RepositoryAccess
}

type Credential struct {
	Value             string    `json:"credential"`
	EnvironmentID     string    `json:"environment_id"`
	MachineID         string    `json:"machine_id"`
	AssignmentID      string    `json:"assignment_id"`
	AssignmentVersion int64     `json:"assignment_version"`
	WarningRevision   string    `json:"warning_revision"`
	ExpiresAt         time.Time `json:"expires_at"`
}

type Lease struct {
	LeaseID       string    `json:"lease_id"`
	RepositoryID  string    `json:"repository_id"`
	AssignmentID  string    `json:"assignment_id"`
	EnvironmentID string    `json:"environment_id"`
	MachineID     string    `json:"machine_id"`
	FencingToken  int64     `json:"fencing_token"`
	BaseRevision  string    `json:"base_remote_revision"`
	ExpiresAt     time.Time `json:"expires_at"`
}

type RepositoryBinding struct {
	RepositoryID string `json:"repository_id"`
	URL          string `json:"url"`
}

type RepositoryAccess struct {
	Transport     string    `json:"transport"`
	RepositoryID  string    `json:"repository_id"`
	AssignmentID  string    `json:"assignment_id"`
	EnvironmentID string    `json:"environment_id"`
	MachineID     string    `json:"machine_id"`
	CloneURL      string    `json:"clone_url"`
	PublishURL    string    `json:"publish_url"`
	Branch        string    `json:"branch"`
	Username      string    `json:"username"`
	Password      string    `json:"password"`
	Capability    string    `json:"capability"`
	ExpiresAt     time.Time `json:"expires_at"`
}

type RuntimePolicy struct {
	Format                        string        `json:"format"`
	Revision                      string        `json:"revision"`
	ManifestContract              string        `json:"manifest_contract"`
	ManifestMaxBytes              int           `json:"manifest_max_bytes"`
	ManifestMaxLines              int           `json:"manifest_max_lines"`
	ManifestMaxPatternBytes       int           `json:"manifest_max_pattern_bytes"`
	MaxFileBytes                  int64         `json:"max_file_bytes"`
	MaxBatchBytes                 int64         `json:"max_batch_bytes"`
	Debounce                      time.Duration `json:"debounce"`
	MinimumPushInterval           time.Duration `json:"minimum_push_interval"`
	MaximumDirtyDelay             time.Duration `json:"maximum_dirty_delay"`
	RemotePollInterval            time.Duration `json:"remote_poll_interval"`
	RetryLimit                    int           `json:"retry_limit"`
	ShutdownFlushTimeout          time.Duration `json:"shutdown_flush_timeout"`
	SummaryLimit                  int           `json:"summary_limit"`
	AbsoluteRuntimeExclusionRoots []string      `json:"-"`
	RuntimeExclusionRoots         []string      `json:"-"`
}

func (p RuntimePolicy) ManifestLimits() ManifestLimits {
	return ManifestLimits{MaxBytes: p.ManifestMaxBytes, MaxLines: p.ManifestMaxLines, MaxPatternBytes: p.ManifestMaxPatternBytes}
}

type RuntimeDescriptor struct {
	AssignmentVersion      int64               `json:"assignment_version"`
	ConfigurationRevision  string              `json:"configuration_revision"`
	PathRules              []PathRule          `json:"path_rules"`
	RepositoryBindings     []RepositoryBinding `json:"repository_bindings"`
	WriteMode              string              `json:"write_mode"`
	Mode                   AssignmentMode      `json:"mode"`
	RepositoryID           string              `json:"repository_id"`
	PullRepositoryID       string              `json:"pull_repository_id,omitempty"`
	PushRepositoryID       string              `json:"push_repository_id,omitempty"`
	AutomaticUpdates       bool                `json:"automatic_updates"`
	ApprovedPullRevision   string              `json:"approved_pull_revision,omitempty"`
	AssignmentID           string              `json:"assignment_id"`
	EnvironmentID          string              `json:"environment_id"`
	MachineID              string              `json:"machine_id"`
	InstallationGeneration int64               `json:"installation_generation"`
	SyncRevisionFloor      int64               `json:"sync_revision_floor"`
	WarningRevision        string              `json:"warning_revision"`
	Policy                 RuntimePolicy       `json:"policy"`
}

func NewControlClient(config ControlClientConfig) (*ControlClient, error) {
	base, err := url.Parse(strings.TrimSpace(config.BaseURL))
	if err != nil || base.Scheme != "https" || base.User != nil || base.Hostname() == "" ||
		base.RawQuery != "" || base.Fragment != "" || config.Identities == nil || config.Proofs == nil ||
		config.OperationID == nil {
		return nil, ErrControlClientInvalid
	}
	allowed := false
	for _, host := range config.AllowedHosts {
		if strings.EqualFold(strings.TrimSpace(host), base.Hostname()) {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, ErrControlClientInvalid
	}
	if config.Timeout <= 0 {
		config.Timeout = 15 * time.Second
	}
	if config.Timeout > time.Minute {
		return nil, ErrControlClientInvalid
	}
	if config.Clock == nil {
		config.Clock = func() time.Time { return time.Now().UTC() }
	}
	repositoryHosts := make(map[string]bool, len(config.RepositoryHosts))
	for _, host := range config.RepositoryHosts {
		host = strings.ToLower(strings.TrimSpace(host))
		if host == "" || strings.ContainsAny(host, "/:@") {
			return nil, ErrControlClientInvalid
		}
		repositoryHosts[host] = true
	}
	transport := config.Transport
	if transport == nil {
		transport = httptransport.Default()
	}
	return &ControlClient{
		base: base, identities: config.Identities, proofs: config.Proofs, operationID: config.OperationID,
		client: &http.Client{
			Transport: errorreport.TransportOperation(transport, base.String(), "config_sync"), Timeout: config.Timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return ErrControlClientInvalid },
		},
		clock: config.Clock, repositoryHosts: repositoryHosts,
	}, nil
}

func (c *ControlClient) Credential(ctx context.Context) (result Credential, resultErr error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.clock().UTC()
	if c.credential.Value != "" && c.credential.ExpiresAt.After(now.Add(30*time.Second)) {
		return c.credential, nil
	}
	body := []byte("{}")
	path := "/v1/config/credentials"
	operationID, err := c.operationID()
	if err != nil {
		return Credential{}, controlFailure(nil, err)
	}
	identity, err := c.identities.Token(ctx)
	if err != nil {
		return Credential{}, controlFailure(nil, err)
	}
	proof, err := c.proofs.Proof(ctx, operationID, http.MethodPost, path, body)
	if err != nil {
		return Credential{}, controlFailure(nil, err)
	}
	request, err := c.request(ctx, path, body, identity, "", proof)
	if err != nil {
		return Credential{}, controlFailure(nil, err)
	}
	response, err := c.client.Do(request)
	if err != nil {
		return Credential{}, controlFailure(nil, err)
	}
	defer func() { resultErr = closeControlBody(response.Body, resultErr) }()
	var envelope struct {
		Data struct {
			Credential        string    `json:"credential"`
			EnvironmentID     string    `json:"environment_id"`
			MachineID         string    `json:"machine_id"`
			AssignmentID      string    `json:"assignment_id"`
			AssignmentVersion int64     `json:"assignment_version"`
			WarningRevision   string    `json:"warning_revision"`
			ExpiresAt         time.Time `json:"expires_at"`
		} `json:"data"`
	}
	if response.StatusCode != http.StatusOK {
		classification := error(ErrControlClientInvalid)
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			classification = ErrAuthorization
		}
		return Credential{}, controlHTTPFailure(response, classification, nil)
	}
	if err := decodeBoundedJSON(response.Body, &envelope); err != nil {
		return Credential{}, controlFailure(ErrControlClientInvalid, err)
	}
	if envelope.Data.Credential == "" || envelope.Data.EnvironmentID == "" || envelope.Data.MachineID == "" ||
		envelope.Data.AssignmentID == "" || envelope.Data.WarningRevision == "" ||
		!envelope.Data.ExpiresAt.After(now) || envelope.Data.ExpiresAt.After(now.Add(5*time.Minute+time.Second)) {
		return Credential{}, controlFailure(ErrControlClientInvalid, ErrControlClientInvalid)
	}
	c.credential = Credential{
		Value: envelope.Data.Credential, EnvironmentID: envelope.Data.EnvironmentID, MachineID: envelope.Data.MachineID,
		AssignmentID: envelope.Data.AssignmentID, AssignmentVersion: envelope.Data.AssignmentVersion, WarningRevision: envelope.Data.WarningRevision, ExpiresAt: envelope.Data.ExpiresAt.UTC(),
	}
	return c.credential, nil
}

func (c *ControlClient) InvalidateCredential() {
	c.mu.Lock()
	c.credential = Credential{}
	c.access = RepositoryAccess{}
	c.accesses = nil
	c.mu.Unlock()
}

// RevalidateCredential forces a fresh eligibility decision without discarding
// repository access that is independently scoped and unexpired. Full
// invalidation still clears both layers after authorization loss or shutdown.
func (c *ControlClient) RevalidateCredential() {
	c.mu.Lock()
	c.credential = Credential{}
	c.mu.Unlock()
}

func (c *ControlClient) RepositoryAccess(ctx context.Context) (RepositoryAccess, error) {
	return c.repositoryAccess(ctx, "")
}

type DirectionalRepositoryAccess struct {
	Client    *ControlClient
	Direction string
}

func (s DirectionalRepositoryAccess) RepositoryAccess(ctx context.Context) (RepositoryAccess, error) {
	if s.Client == nil || (s.Direction != "pull" && s.Direction != "push") {
		return RepositoryAccess{}, ErrControlClientInvalid
	}
	return s.Client.repositoryAccess(ctx, s.Direction)
}

func (c *ControlClient) repositoryAccess(ctx context.Context, direction string) (result RepositoryAccess, resultErr error) {
	c.mu.Lock()
	now := c.clock().UTC()
	cached := c.access
	if direction != "" && c.accesses != nil {
		cached = c.accesses[direction]
	}
	if cached.RepositoryID != "" && cached.ExpiresAt.After(now.Add(time.Minute)) {
		result := cached
		c.mu.Unlock()
		return result, nil
	}
	c.mu.Unlock()
	operationID, err := c.operationID()
	if err != nil {
		return RepositoryAccess{}, controlFailure(nil, err)
	}
	requestBody := struct {
		OperationID string `json:"operation_id"`
		Direction   string `json:"direction,omitempty"`
	}{operationID, direction}
	body, err := json.Marshal(requestBody)
	if err != nil {
		return RepositoryAccess{}, controlFailure(nil, err)
	}
	credential, err := c.Credential(ctx)
	if err != nil {
		return RepositoryAccess{}, err
	}
	identity, err := c.identities.Token(ctx)
	if err != nil {
		return RepositoryAccess{}, controlFailure(nil, err)
	}
	path := "/v1/config/repository-access"
	proof, err := c.proofs.Proof(ctx, operationID, http.MethodPost, path, body)
	if err != nil {
		return RepositoryAccess{}, controlFailure(nil, err)
	}
	request, err := c.request(ctx, path, body, identity, credential.Value, proof)
	if err != nil {
		return RepositoryAccess{}, controlFailure(nil, err)
	}
	response, err := c.client.Do(request)
	if err != nil {
		return RepositoryAccess{}, controlFailure(nil, err)
	}
	defer func() { resultErr = closeControlBody(response.Body, resultErr) }()
	if response.StatusCode != http.StatusOK {
		if isAuthorizationStatus(response.StatusCode) {
			c.InvalidateCredential()
			return RepositoryAccess{}, controlHTTPFailure(response, ErrAuthorization, nil)
		}
		return RepositoryAccess{}, controlHTTPFailure(response, ErrControlClientInvalid, nil)
	}
	var envelope struct {
		Data RepositoryAccess `json:"data"`
	}
	if err := decodeBoundedJSON(response.Body, &envelope); err != nil {
		return RepositoryAccess{}, controlFailure(ErrControlClientInvalid, err)
	}
	if !c.validRepositoryAccess(envelope.Data, credential, now) {
		return RepositoryAccess{}, controlFailure(ErrControlClientInvalid, ErrControlClientInvalid)
	}
	c.mu.Lock()
	if direction == "" {
		c.access = envelope.Data
	} else {
		if c.accesses == nil {
			c.accesses = make(map[string]RepositoryAccess)
		}
		c.accesses[direction] = envelope.Data
	}
	c.mu.Unlock()
	return envelope.Data, nil
}

func (c *ControlClient) RuntimeDescriptor(ctx context.Context) (result RuntimeDescriptor, resultErr error) {
	credential, err := c.Credential(ctx)
	if err != nil {
		return RuntimeDescriptor{}, err
	}
	operationID, err := c.operationID()
	if err != nil {
		return RuntimeDescriptor{}, controlFailure(nil, err)
	}
	body := []byte("{}")
	path := "/v1/config/runtime"
	identity, err := c.identities.Token(ctx)
	if err != nil {
		return RuntimeDescriptor{}, controlFailure(nil, err)
	}
	proof, err := c.proofs.Proof(ctx, operationID, http.MethodPost, path, body)
	if err != nil {
		return RuntimeDescriptor{}, controlFailure(nil, err)
	}
	request, err := c.request(ctx, path, body, identity, credential.Value, proof)
	if err != nil {
		return RuntimeDescriptor{}, controlFailure(nil, err)
	}
	response, err := c.client.Do(request)
	if err != nil {
		return RuntimeDescriptor{}, controlFailure(nil, err)
	}
	defer func() { resultErr = closeControlBody(response.Body, resultErr) }()
	if response.StatusCode != http.StatusOK {
		if isAuthorizationStatus(response.StatusCode) {
			c.InvalidateCredential()
			return RuntimeDescriptor{}, controlHTTPFailure(response, ErrAuthorization, nil)
		}
		return RuntimeDescriptor{}, controlHTTPFailure(response, ErrControlClientInvalid, nil)
	}
	var envelope struct {
		Data RuntimeDescriptor `json:"data"`
	}
	if err := decodeBoundedJSON(response.Body, &envelope); err != nil {
		return RuntimeDescriptor{}, controlFailure(ErrControlClientInvalid, err)
	}
	if err := validateRuntimeDescriptor(envelope.Data, credential); err != nil {
		return RuntimeDescriptor{}, controlFailure(ErrControlClientInvalid, err)
	}
	return envelope.Data, nil
}

func (c *ControlClient) Pending(ctx context.Context) (result []ConflictResolution, resultErr error) {
	body := []byte("{}")
	path := "/v1/config/conflict-resolutions/pending"
	response, err := c.authorizedConfigRequest(ctx, path, body)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = closeControlBody(response.Body, resultErr) }()
	var envelope struct {
		Data struct {
			Items []ConflictResolution `json:"items"`
		} `json:"data"`
	}
	if response.StatusCode != http.StatusOK {
		return nil, controlHTTPFailure(response, ErrControlClientInvalid, nil)
	}
	if err := decodeBoundedJSON(response.Body, &envelope); err != nil {
		return nil, controlFailure(ErrControlClientInvalid, err)
	}
	if len(envelope.Data.Items) > 100 {
		return nil, controlFailure(ErrControlClientInvalid, ErrControlClientInvalid)
	}
	for _, item := range envelope.Data.Items {
		if !item.Valid() {
			return nil, controlFailure(ErrControlClientInvalid, ErrControlClientInvalid)
		}
	}
	return envelope.Data.Items, nil
}

func (c *ControlClient) Acknowledge(ctx context.Context, id, landedRevision string) (resultErr error) {
	body, err := json.Marshal(struct {
		ID             string `json:"id"`
		LandedRevision string `json:"landed_revision"`
	}{ID: id, LandedRevision: landedRevision})
	if err != nil {
		return controlFailure(ErrControlClientInvalid, err)
	}
	response, err := c.authorizedConfigRequest(ctx, "/v1/config/conflict-resolutions/acknowledge", body)
	if err != nil {
		return err
	}
	defer func() { resultErr = closeControlBody(response.Body, resultErr) }()
	var envelope struct {
		Data struct {
			Applied bool `json:"applied"`
		} `json:"data"`
	}
	if response.StatusCode != http.StatusOK {
		return controlHTTPFailure(response, ErrControlClientInvalid, nil)
	}
	if err := decodeBoundedJSON(response.Body, &envelope); err != nil {
		if response.StatusCode >= 400 {
			return controlHTTPFailure(response, ErrControlClientInvalid, err)
		}
		return controlFailure(ErrControlClientInvalid, err)
	}
	if !envelope.Data.Applied {
		return controlFailure(ErrControlClientInvalid, ErrControlClientInvalid)
	}
	return nil
}

func (c *ControlClient) authorizedConfigRequest(ctx context.Context, path string, body []byte) (*http.Response, error) {
	operationID, err := c.operationID()
	if err != nil {
		return nil, controlFailure(nil, err)
	}
	credential, err := c.Credential(ctx)
	if err != nil {
		return nil, err
	}
	identity, err := c.identities.Token(ctx)
	if err != nil {
		return nil, controlFailure(nil, err)
	}
	proof, err := c.proofs.Proof(ctx, operationID, http.MethodPost, path, body)
	if err != nil {
		return nil, controlFailure(nil, err)
	}
	request, err := c.request(ctx, path, body, identity, credential.Value, proof)
	if err != nil {
		return nil, controlFailure(nil, err)
	}
	response, err := c.client.Do(request)
	if err != nil {
		return nil, controlFailure(nil, err)
	}
	if isAuthorizationStatus(response.StatusCode) {
		closeErr := response.Body.Close()
		c.InvalidateCredential()
		return nil, controlHTTPFailure(response, ErrAuthorization, closeErr)
	}
	return response, nil
}

func (c *ControlClient) validRepositoryAccess(access RepositoryAccess, credential Credential, now time.Time) bool {
	if access.RepositoryID == "" || access.AssignmentID != credential.AssignmentID || access.EnvironmentID != credential.EnvironmentID || access.MachineID != credential.MachineID ||
		access.Branch == "" || len(access.Branch) > 255 || (access.Capability != "repository_contents_read" && access.Capability != "repository_contents_write") ||
		!access.ExpiresAt.After(now.Add(time.Minute)) || access.ExpiresAt.After(now.Add(time.Hour+time.Minute)) {
		return false
	}
	switch access.Transport {
	case "https", "http", "ssh", "local":
	default:
		return false
	}
	if access.Password != "" {
		if access.Transport != "https" || access.Username != "x-access-token" || len(access.Password) > 4096 {
			return false
		}
	} else if access.Username != "" {
		return false
	}
	for _, raw := range []string{access.CloneURL, access.PublishURL} {
		_, kind, err := NormalizeRepositoryEndpoint(raw)
		if err != nil || kind != access.Transport {
			return false
		}
		if access.Password != "" {
			parsed, err := url.Parse(raw)
			if err != nil || !c.repositoryHosts[strings.ToLower(parsed.Hostname())] {
				return false
			}
		}
	}
	return true
}

func (c *ControlClient) AcquireLease(ctx context.Context, baseRevision string, ttl time.Duration) (Lease, error) {
	operationID, err := c.operationID()
	if err != nil {
		return Lease{}, controlFailure(nil, err)
	}
	return c.acquireLease(ctx, operationID, baseRevision, ttl)
}

func (c *ControlClient) acquireLease(ctx context.Context, operationID, baseRevision string, ttl time.Duration) (Lease, error) {
	baseRevision = strings.TrimSpace(baseRevision)
	body, err := json.Marshal(struct {
		OperationID        string `json:"operation_id"`
		BaseRemoteRevision string `json:"base_remote_revision"`
		TTLSeconds         int64  `json:"ttl_seconds"`
	}{operationID, baseRevision, int64(ttl / time.Second)})
	if err != nil {
		return Lease{}, controlFailure(nil, err)
	}
	lease, err := c.leaseRequest(ctx, "/v1/config/leases/acquire", operationID, body)
	if err == nil && lease.BaseRevision != baseRevision {
		return Lease{}, ErrControlClientInvalid
	}
	return lease, err
}

func (c *ControlClient) RenewLease(ctx context.Context, lease Lease, ttl time.Duration) (Lease, error) {
	operationID, err := c.operationID()
	if err != nil {
		return Lease{}, controlFailure(nil, err)
	}
	body, err := json.Marshal(struct {
		OperationID  string `json:"operation_id"`
		LeaseID      string `json:"lease_id"`
		FencingToken int64  `json:"fencing_token"`
		TTLSeconds   int64  `json:"ttl_seconds"`
	}{operationID, lease.LeaseID, lease.FencingToken, int64(ttl / time.Second)})
	if err != nil {
		return Lease{}, controlFailure(nil, err)
	}
	renewed, err := c.leaseRequest(ctx, "/v1/config/leases/renew", operationID, body)
	if err == nil && (renewed.LeaseID != lease.LeaseID || renewed.RepositoryID != lease.RepositoryID ||
		renewed.AssignmentID != lease.AssignmentID || renewed.EnvironmentID != lease.EnvironmentID ||
		renewed.MachineID != lease.MachineID || renewed.FencingToken != lease.FencingToken ||
		renewed.BaseRevision != lease.BaseRevision) {
		return Lease{}, ErrControlClientInvalid
	}
	return renewed, err
}

func (c *ControlClient) ReleaseLease(ctx context.Context, lease Lease) error {
	operationID, err := c.operationID()
	if err != nil {
		return controlFailure(nil, err)
	}
	body, err := json.Marshal(struct {
		OperationID  string `json:"operation_id"`
		LeaseID      string `json:"lease_id"`
		FencingToken int64  `json:"fencing_token"`
	}{operationID, lease.LeaseID, lease.FencingToken})
	if err != nil {
		return controlFailure(nil, err)
	}
	_, err = c.leaseRequest(ctx, "/v1/config/leases/release", operationID, body)
	return err
}

func (c *ControlClient) ReportStatus(ctx context.Context, status Status, summaryLimit int) (resultErr error) {
	if status.Validate(summaryLimit) != nil {
		return ErrControlClientInvalid
	}
	body, err := json.Marshal(status)
	if err != nil {
		return controlFailure(nil, err)
	}
	operationID, err := c.operationID()
	if err != nil {
		return controlFailure(nil, err)
	}
	path := "/v1/config/status"
	identity, err := c.identities.Token(ctx)
	if err != nil {
		return controlFailure(nil, err)
	}
	proof, err := c.proofs.Proof(ctx, operationID, http.MethodPost, path, body)
	if err != nil {
		return controlFailure(nil, err)
	}
	request, err := c.request(ctx, path, body, identity, "", proof)
	if err != nil {
		return controlFailure(nil, err)
	}
	response, err := c.client.Do(request)
	if err != nil {
		return controlFailure(nil, err)
	}
	defer func() { resultErr = closeControlBody(response.Body, resultErr) }()
	var envelope struct {
		Data struct {
			SyncRevision int64 `json:"sync_revision"`
		} `json:"data"`
		Error struct {
			Code      string         `json:"code"`
			Message   string         `json:"message"`
			RequestID string         `json:"request_id"`
			Details   map[string]any `json:"details"`
		} `json:"error,omitempty"`
	}
	if err := decodeBoundedJSON(response.Body, &envelope); err != nil {
		if isAuthorizationStatus(response.StatusCode) {
			c.InvalidateCredential()
			return controlHTTPFailure(response, ErrAuthorization, err)
		}
		if response.StatusCode >= 400 {
			return controlHTTPFailure(response, ErrControlClientInvalid, err)
		}
		return controlFailure(ErrControlClientInvalid, err)
	}
	if response.StatusCode == http.StatusConflict && envelope.Error.Code == "status_revision_stale" {
		return controlHTTPFailure(response, ErrOperationConflict, nil)
	}
	if response.StatusCode != http.StatusAccepted || envelope.Data.SyncRevision != status.SyncRevision {
		if isAuthorizationStatus(response.StatusCode) {
			c.InvalidateCredential()
			return controlHTTPFailure(response, ErrAuthorization, nil)
		}
		if response.StatusCode != http.StatusAccepted {
			return controlHTTPFailure(response, ErrControlClientInvalid, nil)
		}
		return controlFailure(ErrControlClientInvalid, ErrControlClientInvalid)
	}
	return nil
}

func (c *ControlClient) leaseRequest(ctx context.Context, path, operationID string, body []byte) (result Lease, resultErr error) {
	credential, err := c.Credential(ctx)
	if err != nil {
		return Lease{}, err
	}
	identity, err := c.identities.Token(ctx)
	if err != nil {
		return Lease{}, controlFailure(nil, err)
	}
	proof, err := c.proofs.Proof(ctx, operationID, http.MethodPost, path, body)
	if err != nil {
		return Lease{}, controlFailure(nil, err)
	}
	request, err := c.request(ctx, path, body, identity, credential.Value, proof)
	if err != nil {
		return Lease{}, controlFailure(nil, err)
	}
	response, err := c.client.Do(request)
	if err != nil {
		return Lease{}, controlFailure(nil, err)
	}
	defer func() { resultErr = closeControlBody(response.Body, resultErr) }()
	if response.StatusCode != http.StatusOK {
		var envelope struct {
			Error struct {
				Code      string         `json:"code"`
				Message   string         `json:"message"`
				RequestID string         `json:"request_id"`
				Details   map[string]any `json:"details"`
			} `json:"error"`
		}
		decodeErr := decodeBoundedJSON(response.Body, &envelope)
		switch envelope.Error.Code {
		case "config_writes_disabled":
			return Lease{}, controlHTTPFailure(response, ErrWritesDisabled, decodeErr)
		case "lease_busy":
			return Lease{}, controlHTTPFailure(response, ErrLeaseBusy, decodeErr)
		case "lease_lost":
			return Lease{}, controlHTTPFailure(response, ErrLeaseLost, decodeErr)
		case "operation_conflict":
			return Lease{}, controlHTTPFailure(response, ErrOperationConflict, decodeErr)
		default:
			if isAuthorizationStatus(response.StatusCode) {
				c.InvalidateCredential()
				return Lease{}, controlHTTPFailure(response, ErrAuthorization, decodeErr)
			}
			return Lease{}, controlHTTPFailure(response, ErrControlClientInvalid, decodeErr)
		}
	}
	if path == "/v1/config/leases/release" {
		var envelope struct {
			Data struct {
				Released bool `json:"released"`
			} `json:"data"`
		}
		if err := decodeBoundedJSON(response.Body, &envelope); err != nil {
			return Lease{}, controlFailure(ErrControlClientInvalid, err)
		}
		if !envelope.Data.Released {
			return Lease{}, controlFailure(ErrControlClientInvalid, ErrControlClientInvalid)
		}
		return Lease{}, nil
	}
	var envelope struct {
		Data Lease `json:"data"`
	}
	if err := decodeBoundedJSON(response.Body, &envelope); err != nil {
		return Lease{}, controlFailure(ErrControlClientInvalid, err)
	}
	if envelope.Data.LeaseID == "" ||
		envelope.Data.RepositoryID == "" || envelope.Data.FencingToken < 1 ||
		envelope.Data.AssignmentID != credential.AssignmentID ||
		envelope.Data.EnvironmentID != credential.EnvironmentID || envelope.Data.MachineID != credential.MachineID ||
		!envelope.Data.ExpiresAt.After(c.clock().UTC()) {
		return Lease{}, controlFailure(ErrControlClientInvalid, ErrControlClientInvalid)
	}
	return envelope.Data, nil
}

func (c *ControlClient) request(ctx context.Context, path string, body []byte, identity, credential string, proof []byte) (*http.Request, error) {
	endpoint := c.base.ResolveReference(&url.URL{Path: path})
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Paperboat-Machine-Proof", base64.RawURLEncoding.EncodeToString(proof))
	if credential == "" {
		request.Header.Set("Authorization", "Bearer "+identity)
	} else {
		request.Header.Set("Authorization", "Bearer "+credential)
		request.Header.Set("X-Paperboat-Machine-Identity", identity)
	}
	return request, nil
}

func decodeBoundedJSON(reader io.Reader, target any) error {
	data, err := io.ReadAll(io.LimitReader(reader, maxControlResponseBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxControlResponseBytes {
		return ErrControlClientInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return ErrControlClientInvalid
	}
	return nil
}

type controlRequestFailure struct {
	classification error
	cause          error
}

func (e *controlRequestFailure) Error() string {
	if e == nil || e.classification == nil {
		return "config sync control request failed"
	}
	return e.classification.Error()
}

func (e *controlRequestFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *controlRequestFailure) Is(target error) bool {
	return e != nil && e.classification != nil && target == e.classification
}

func (*controlRequestFailure) DiagnosticStage() string { return "control_request" }
func (*controlRequestFailure) DiagnosticCode() string  { return "control_request_failed" }

func controlFailure(classification, cause error) error {
	if cause == nil {
		cause = classification
	}
	return &controlRequestFailure{classification: classification, cause: cause}
}

func controlHTTPFailure(response *http.Response, classification, additionalCause error) error {
	cause := errorreport.HTTPStatusFailure(response)
	if additionalCause != nil {
		cause = errors.Join(cause, additionalCause)
	}
	return controlFailure(classification, cause)
}

func closeControlBody(body io.ReadCloser, previous error) error {
	if body == nil {
		return previous
	}
	closeErr := body.Close()
	if closeErr == nil {
		return previous
	}
	classification := controlFailureClassification(previous)
	return controlFailure(classification, errors.Join(previous, closeErr))
}

func controlFailureClassification(err error) error {
	for _, candidate := range []error{
		ErrAuthorization, ErrLeaseBusy, ErrLeaseLost, ErrOperationConflict, ErrWritesDisabled, ErrControlClientInvalid,
	} {
		if errors.Is(err, candidate) {
			return candidate
		}
	}
	return nil
}

func isAuthorizationStatus(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}

func expectedAuthorizationDenial(err error) bool {
	if !errors.Is(err, ErrAuthorization) {
		return false
	}
	fault := errorreport.ProjectFault(context.Background(), configSyncComponent, configSyncOperation, "control_request", "control_request_failed", err)
	return fault.Outcome == "rejected" && isAuthorizationStatus(fault.HTTPStatus)
}
