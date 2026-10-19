package api

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/machinename"
)

type Pagination struct {
	Limit      int  `json:"limit"`
	Offset     int  `json:"offset"`
	Total      int  `json:"total"`
	NextOffset *int `json:"next_offset"`
}

// UserMachine describes an enrolled machine. The control plane owns its lifecycle
// and authorization; the CLI uses this metadata to select and manage it.
type UserMachine struct {
	Ownership     string   `json:"ownership"`
	OwnerAccount  string   `json:"owner_account,omitempty"`
	OwnerTeamID   string   `json:"owner_team_id,omitempty"`
	Permissions   []string `json:"permissions"`
	CanManage     bool     `json:"can_manage"`
	Shared        bool     `json:"shared"`
	ID            string   `json:"id"`
	EnvironmentID string   `json:"environment_id"`
	Description   string   `json:"description"`
	Alias         string   `json:"alias"`
	State         string   `json:"state"`
	Online        bool     `json:"online"`
	Platform      string   `json:"platform"`
	Architecture  string   `json:"architecture"`
	WorkspaceRoot string   `json:"workspace_root"`

	Capabilities           MachineCapabilities     `json:"capabilities"`
	MachineCapabilities    MachineCapabilityPolicy `json:"machine_capabilities"`
	PublicIdentityKey      string                  `json:"public_identity_key"`
	AssignedIP             string                  `json:"assigned_ip,omitempty"`
	NetworkApproved        bool                    `json:"network_approved"`
	InstallationGeneration int64                   `json:"installation_generation"`
	Availability           AvailabilityPolicy      `json:"availability"`
	RuntimeDiagnostics     RuntimeDiagnostics      `json:"runtime_diagnostics"`
	Installation           *ClientInstallation     `json:"installation,omitempty"`
	SSHAuthority           SSHAuthority            `json:"-"`
	SSHLocalReady          bool                    `json:"-"`
	SSHLocalCode           string                  `json:"-"`
	SSHUser                string                  `json:"-"`
	SSHPort                uint16                  `json:"-"`
}

type MachineCapabilitySelection struct {
	Terminal      bool `json:"terminal"`
	ManagedSSH    bool `json:"managed_ssh"`
	FileReceive   bool `json:"file_receive"`
	PreviewTunnel bool `json:"preview_tunnel"`
}

type MachineCapabilityPolicy struct {
	Schema         string                     `json:"schema"`
	Desired        MachineCapabilitySelection `json:"desired"`
	DesiredVersion int64                      `json:"desired_version"`
	Applied        MachineCapabilitySelection `json:"applied"`
	AppliedVersion int64                      `json:"applied_version"`
	Status         string                     `json:"status"`
	ErrorCode      string                     `json:"error_code,omitempty"`
}

type SSHAuthority struct {
	TargetGeneration  uint64
	HostKeyGeneration uint64
}

type ManagedSSHTarget struct {
	Type                  string `json:"type"`
	Version               int    `json:"version"`
	MachineID             string `json:"machine_id"`
	MachineGeneration     uint64 `json:"machine_generation"`
	OSUser                string `json:"os_user"`
	Port                  uint16 `json:"port"`
	ReconciliationVersion uint64 `json:"reconciliation_version"`
}

type ManagedSSHHostKeySet struct {
	Type                  string   `json:"type"`
	Version               int      `json:"version"`
	SetID                 string   `json:"set_id"`
	MachineID             string   `json:"machine_id"`
	MachineGeneration     uint64   `json:"machine_generation"`
	ObservationGeneration uint64   `json:"observation_generation"`
	Keys                  []string `json:"keys"`
	Fingerprint           string   `json:"fingerprint"`
	State                 string   `json:"state"`
	ReconciliationVersion uint64   `json:"reconciliation_version"`
}

type ManagedSSHAuthorizedKeys struct {
	Type              string   `json:"type"`
	Version           int      `json:"version"`
	MachineID         string   `json:"machine_id"`
	MachineGeneration uint64   `json:"machine_generation"`
	Keys              []string `json:"keys"`
}

type ManagedSSHClientKey struct {
	Type                  string `json:"type"`
	Version               int    `json:"version"`
	Fingerprint           string `json:"fingerprint"`
	PublicKey             string `json:"public_key"`
	State                 string `json:"state"`
	ReconciliationVersion uint64 `json:"reconciliation_version"`
}

type ClientInstallation struct {
	ControlURL          string `json:"control_url"`
	HelperListenAddress string `json:"helper_listen_address"`
}

type MachineCapability struct {
	Configured bool `json:"configured"`
	Observed   bool `json:"observed"`
}

type MachineCapabilities struct {
	FileReceive          MachineCapability `json:"file_receive"`
	PreviewLaunch        MachineCapability `json:"preview_launch"`
	TerminalHost         MachineCapability `json:"terminal_host"`
	SessionHost          MachineCapability `json:"session_host"`
	KeepAwake            MachineCapability `json:"keep_awake"`
	EnvironmentInjection MachineCapability `json:"environment_injection"`
}

type MachineSetupInput struct {
	Alias             string            `json:"alias"`
	Platform          string            `json:"platform"`
	Architecture      string            `json:"architecture"`
	WorkspaceRoot     string            `json:"workspace_root"`
	PublicIdentityKey string            `json:"public_identity_key"`
	RuntimeVersions   map[string]string `json:"runtime_versions"`
}

type MachineEnrollmentStart struct {
	ID             string    `json:"id"`
	BootstrapToken string    `json:"bootstrap_token"`
	ServerURL      string    `json:"server_url"`
	ExpiresAt      time.Time `json:"expires_at"`
}

// StartMachineEnrollment creates the single-use credential used by the
// dashboard, CLI, and TUI one-shot installers.
func (c *Client) StartMachineEnrollment(ctx context.Context, idempotencyKey, shell string) (MachineEnrollmentStart, error) {
	var out MachineEnrollmentStart
	if strings.TrimSpace(idempotencyKey) == "" {
		return out, errors.New("machine enrollment idempotency key is required")
	}
	if shell != "posix" && shell != "powershell" {
		return out, errors.New("machine enrollment shell must be posix or powershell")
	}
	err := c.doWithHeaders(ctx, http.MethodPost, "/v1/machine-enrollments", map[string]string{"shell": shell}, &out, http.Header{"Idempotency-Key": []string{idempotencyKey}})
	return out, err
}

func (c *Client) CancelMachineEnrollment(ctx context.Context, id, key string) error {
	return c.doWithHeaders(ctx, http.MethodPost, "/v1/machine-enrollments/"+url.PathEscape(id)+"/cancel", nil, nil, http.Header{"Idempotency-Key": []string{key}})
}

func (c *Client) SetupMachine(ctx context.Context, input MachineSetupInput) (UserMachine, error) {
	var out UserMachine
	input.Alias = strings.ToLower(strings.TrimSpace(input.Alias))
	err := c.do(ctx, http.MethodPost, "/v1/machines/setup", input, &out)
	return out, err
}

type AuthenticatedMachineInstallationInput struct {
	Verifier               string `json:"verifier"`
	PublicIdentityKey      string `json:"public_identity_key"`
	InstallationGeneration int64  `json:"installation_generation"`

	SSHUser                 string `json:"ssh_user,omitempty"`
	SSHPort                 uint16 `json:"ssh_port,omitempty"`
	CanReuseRuntimeIdentity bool   `json:"can_reuse_runtime_identity,omitempty"`
}

type AuthenticatedMachineInstallation struct {
	ExpiresAt time.Time `json:"expires_at"`
}

func (c *Client) PrepareAuthenticatedMachineInstallation(ctx context.Context, machineID, operationID string, input AuthenticatedMachineInstallationInput) (AuthenticatedMachineInstallation, error) {
	if strings.TrimSpace(machineID) == "" || len(strings.TrimSpace(operationID)) < 8 {
		return AuthenticatedMachineInstallation{}, errors.New("authenticated machine installation request is invalid")
	}
	var out AuthenticatedMachineInstallation
	err := c.doWithHeaders(ctx, http.MethodPost, "/v1/machines/"+url.PathEscape(machineID)+"/machine-installations", input, &out, http.Header{"Idempotency-Key": []string{operationID}})
	return out, err
}

type MachineControlCredential struct {
	Credential string    `json:"credential"`
	ExpiresAt  time.Time `json:"expires_at"`
}

func (c *Client) IssueMachineControlCredential(ctx context.Context, machineID, operationID string, proof []byte) (MachineControlCredential, error) {
	if strings.TrimSpace(machineID) == "" || len(operationID) < 8 || len(proof) == 0 {
		return MachineControlCredential{}, errors.New("machine control credential request is invalid")
	}
	var out MachineControlCredential
	path := "/v1/machines/" + url.PathEscape(machineID) + "/control-credentials"
	err := c.doWithHeaders(ctx, http.MethodPost, path, struct {
		OperationID string `json:"operation_id"`
	}{operationID}, &out, http.Header{"X-Paperboat-Machine-Proof": []string{base64.RawURLEncoding.EncodeToString(proof)}})
	return out, err
}

type RuntimeDiagnostics struct {
	WorkerGeneration    uint64     `json:"worker_generation"`
	OSBootID            string     `json:"os_boot_id,omitempty"`
	WorkerServiceScope  string     `json:"worker_service_scope"`
	ConnectorState      string     `json:"connector_state"`
	ConnectorGeneration uint64     `json:"connector_generation"`
	ObservedAt          *time.Time `json:"observed_at,omitempty"`
}

type UserMachinePage struct {
	Items      []UserMachine `json:"items"`
	Pagination Pagination    `json:"pagination"`
}

// Me fetches the authenticated user, validating the reused credential.
func (c *Client) Me(ctx context.Context) (Me, error) {
	var out Me
	err := c.do(ctx, http.MethodGet, "/v1/me", nil, &out)
	return out, err
}

// ListUserMachines returns every enrolled machine page using the
// server-authored cursor. Calling it never reveals connector credentials or
// local paths beyond the machine's declared scope.
func (c *Client) ListUserMachines(ctx context.Context) ([]UserMachine, error) {
	return c.ListUserMachinesFiltered(ctx, nil)
}

func (c *Client) ListUserMachinesFiltered(ctx context.Context, filters url.Values) ([]UserMachine, error) {
	query := url.Values{"sort": {"alias"}}
	for _, key := range []string{"q", "state", "owner"} {
		if value := filters.Get(key); value != "" {
			query.Set(key, value)
		}
	}
	return collectOffsetInventory[UserMachine](ctx, c, "/v1/machines", "items", query)
}

func (c *Client) DisconnectUserMachine(ctx context.Context, machineID string) error {
	if strings.TrimSpace(machineID) == "" {
		return errors.New("machine ID is required")
	}
	return c.do(ctx, http.MethodPost, "/v1/machines/"+url.PathEscape(machineID)+"/disconnect", nil, nil)
}

func (c *Client) RenameUserMachine(ctx context.Context, machineID, alias string) (UserMachine, error) {
	machineID = strings.TrimSpace(machineID)
	alias = strings.ToLower(strings.TrimSpace(alias))
	if machineID == "" || alias == "" {
		return UserMachine{}, errors.New("machine ID and alias are required")
	}
	if err := machinename.Validate(alias); err != nil {
		return UserMachine{}, err
	}
	var out UserMachine
	err := c.do(ctx, http.MethodPatch, "/v1/machines/"+url.PathEscape(machineID), map[string]string{"alias": alias}, &out)
	return out, err
}

func (c *Client) SetUserMachineAvailability(ctx context.Context, machineID, mode, idempotencyKey string, expectedVersion int64) (AvailabilityPolicy, error) {
	if strings.TrimSpace(machineID) == "" || (mode != "allow_sleep" && mode != "keep_awake") || strings.TrimSpace(idempotencyKey) == "" || expectedVersion < 0 {
		return AvailabilityPolicy{}, errors.New("valid machine availability input is required")
	}
	var out AvailabilityPolicy
	path := "/v1/machines/" + url.PathEscape(machineID) + "/availability-policy"
	err := c.doWithHeaders(ctx, http.MethodPut, path, map[string]any{"expected_version": expectedVersion, "mode": mode}, &out, http.Header{"Idempotency-Key": []string{idempotencyKey}})
	return out, err
}

func (c *Client) SetUserMachineCapabilities(ctx context.Context, machineID, idempotencyKey string, desired MachineCapabilitySelection, expectedVersion int64) (MachineCapabilityPolicy, error) {
	if strings.TrimSpace(machineID) == "" || strings.TrimSpace(idempotencyKey) == "" || expectedVersion < 1 {
		return MachineCapabilityPolicy{}, errors.New("valid machine capability input is required")
	}
	var out MachineCapabilityPolicy
	path := "/v1/machines/" + url.PathEscape(machineID) + "/capabilities"
	err := c.doWithHeaders(ctx, http.MethodPut, path, map[string]any{"expected_version": expectedVersion, "desired": desired}, &out, http.Header{"Idempotency-Key": []string{idempotencyKey}})
	return out, err
}

func (c *Client) DeleteUserMachine(ctx context.Context, machineID string) error {
	if strings.TrimSpace(machineID) == "" {
		return errors.New("machine ID is required")
	}
	return c.do(ctx, http.MethodDelete, "/v1/machines/"+url.PathEscape(machineID), nil, nil)
}
