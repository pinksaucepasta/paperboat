package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/remotepath"
)

type TransferDestinationDefault struct {
	Configured bool         `json:"configured"`
	Machine    *UserMachine `json:"machine"`
}

func (c *Client) TransferDestinationDefault(ctx context.Context) (TransferDestinationDefault, error) {
	var out TransferDestinationDefault
	err := c.do(ctx, http.MethodGet, "/v1/transfer-destination-default", nil, &out)
	return out, err
}

func (c *Client) SetTransferDestinationDefault(ctx context.Context, machineID string) (TransferDestinationDefault, error) {
	var out TransferDestinationDefault
	err := c.do(ctx, http.MethodPut, "/v1/transfer-destination-default", map[string]string{"machine_id": machineID}, &out)
	return out, err
}

func (c *Client) ClearTransferDestinationDefault(ctx context.Context) error {
	return c.do(ctx, http.MethodDelete, "/v1/transfer-destination-default", nil, nil)
}

func (c *Client) TerminalSessionTransferDestination(ctx context.Context, sessionID string) (TransferDestinationDefault, error) {
	var out TransferDestinationDefault
	err := c.do(ctx, http.MethodGet, "/v1/terminal-sessions/"+url.PathEscape(sessionID)+"/transfer-destination", nil, &out)
	return out, err
}

func (c *Client) SetTerminalSessionTransferDestination(ctx context.Context, sessionID, machineID string) (TransferDestinationDefault, error) {
	var out TransferDestinationDefault
	err := c.do(ctx, http.MethodPut, "/v1/terminal-sessions/"+url.PathEscape(sessionID)+"/transfer-destination", map[string]string{"machine_id": machineID}, &out)
	return out, err
}

func (c *Client) ClearTerminalSessionTransferDestination(ctx context.Context, sessionID string) error {
	return c.do(ctx, http.MethodDelete, "/v1/terminal-sessions/"+url.PathEscape(sessionID)+"/transfer-destination", nil, nil)
}

func (c *Client) EligibleTerminalSessionTransferDestinations(ctx context.Context, sessionID string) ([]UserMachine, error) {
	var page struct {
		Items []UserMachine `json:"items"`
	}
	err := c.do(ctx, http.MethodGet, "/v1/terminal-sessions/"+url.PathEscape(sessionID)+"/transfer-destinations", nil, &page)
	return page.Items, err
}

// TerminalSession is the durable session catalog record returned by the
// control plane. Runtime-only fields may be unavailable while a VM is stopped.
type TerminalSession struct {
	Title             string           `json:"title"`
	CurrentDirectory  string           `json:"current_directory,omitempty"`
	ForegroundProcess string           `json:"foreground_process,omitempty"`
	StartedIn         string           `json:"started_in"`
	Machine           string           `json:"machine"`
	ID                string           `json:"id"`
	Name              string           `json:"name"`
	IsDefault         bool             `json:"is_default"`
	State             string           `json:"state"`
	AttachedCount     *int             `json:"attached_count"`
	LastActiveAt      *time.Time       `json:"last_active_at"`
	CreatedAt         time.Time        `json:"created_at"`
	UpdatedAt         time.Time        `json:"updated_at"`
	EvictedSession    *TerminalSession `json:"evicted_session,omitempty"`
}

type TerminalSessionPage struct {
	Items      []TerminalSession `json:"items"`
	Pagination Pagination        `json:"pagination"`
}

// AuthMaterial is short-lived auth material scoped by paperboat-server for a
// specific connect descriptor. The protocol contract defines the exact token format.
type AuthMaterial struct {
	Method          string    `json:"method"`
	Ticket          string    `json:"ticket,omitempty"`
	Token           string    `json:"token,omitempty"`
	ExpiresAt       time.Time `json:"expires_at"`
	Scopes          []string  `json:"scopes,omitempty"`
	AccessSessionID string    `json:"access_session_id,omitempty"`
}

const ConnectionSchemaV1 = "paperboat.environment-connection/v1"

// Environment identifies the enrolled machine behind a terminal descriptor.
type Environment struct {
	ID            string `json:"id"`
	Kind          string `json:"kind"`
	ResourceID    string `json:"resource_id"`
	State         string `json:"state"`
	Root          string `json:"root"`
	EnvironmentID string `json:"environment_id"`
	UserMachineID string `json:"machine_id"`
	Alias         string `json:"alias"`
	ProjectRoot   string `json:"project_root"`
}

// Terminal is the CLI-safe Paperboat WebSocket attach descriptor from
// cli-connect. It carries client-safe Paperboat route URLs, not raw VM
// addresses or SSH credentials.
type Terminal struct {
	Protocol  string            `json:"protocol"`
	Endpoints TerminalEndpoints `json:"endpoints"`
	SessionID string            `json:"session_id"`
	Auth      AuthMaterial      `json:"auth"`
	CWD       string            `json:"cwd"`
}

type TerminalEndpoints struct {
	QUIC string `json:"quic"`
	WSS  string `json:"wss"`
}

type FileTransferPolicy struct {
	Revision               string `json:"revision"`
	MaxFileBytes           int64  `json:"max_file_bytes"`
	MaxBatchFiles          int    `json:"max_batch_files"`
	MaxBatchBytes          int64  `json:"max_batch_bytes"`
	MaxConcurrentTransfers int    `json:"max_concurrent_transfers"`
	RetentionSeconds       int64  `json:"retention_seconds"`
	DeliveryTimeoutSeconds int64  `json:"delivery_timeout_seconds"`
	MaxPendingSpoolBytes   int64  `json:"max_pending_spool_bytes"`
	AutomaticMaxBatchBytes int64  `json:"automatic_max_batch_bytes,omitempty"`
}

// AutomaticTransferLimit is the largest Automatic transfer: 2,000,000,000 bytes (PLAN 3b, File transfer modes).
const AutomaticTransferLimit = int64(2_000_000_000)

// Bounds is the per-file and per-batch byte bound: the Automatic batch bound for both when the server sends it,
// otherwise the earlier separate fields (accepted until the server stops sending them).
func (p FileTransferPolicy) Bounds() (file, batch int64) {
	if p.AutomaticMaxBatchBytes > 0 {
		return p.AutomaticMaxBatchBytes, p.AutomaticMaxBatchBytes
	}
	return p.MaxFileBytes, p.MaxBatchBytes
}

func (p FileTransferPolicy) ValidBounds() bool {
	file, batch := p.Bounds()
	return file > 0 && file <= batch && batch <= AutomaticTransferLimit && p.MaxPendingSpoolBytes >= batch
}

type FileTransfer struct {
	Endpoint             string             `json:"endpoint"`
	SourceMachineID      string             `json:"source_machine_id"`
	DestinationMachineID string             `json:"destination_machine_id"`
	InitiatingUserID     string             `json:"initiating_user_id"`
	Auth                 AuthMaterial       `json:"auth"`
	Policy               FileTransferPolicy `json:"policy"`
}

// ConnectionDescriptor is the cli-connect / connection-status descriptor. When
// Connectable is false the machine is not ready yet; Status/Reason explain why
// and the caller should poll ConnectionReadiness.
type ConnectionDescriptor struct {
	MachineGeneration uint64        `json:"machine_generation,omitempty"`
	Schema            string        `json:"schema"`
	Issuer            string        `json:"issuer,omitempty"`
	UserMachineID     string        `json:"machine_id"`
	UserMachineState  string        `json:"machine_state"`
	Connectable       bool          `json:"connectable"`
	ExpiresAt         time.Time     `json:"expires_at"`
	Environment       *Environment  `json:"environment,omitempty"`
	Terminal          *Terminal     `json:"terminal,omitempty"`
	FileTransfer      *FileTransfer `json:"file_transfer,omitempty"`
	Status            string        `json:"status,omitempty"`
	Reason            string        `json:"reason,omitempty"`
	RetryAfterSeconds int           `json:"retry_after_seconds,omitempty"`
	Capabilities      []string      `json:"capabilities,omitempty"`
}

type ExecDescriptor struct {
	OperationID string            `json:"operation_id"`
	Environment *Environment      `json:"environment"`
	Endpoints   TerminalEndpoints `json:"endpoints"`
	Auth        AuthMaterial      `json:"auth"`
	ExpiresAt   time.Time         `json:"expires_at"`
}

type SSHDescriptor = ExecDescriptor

// NormalizeConnectionDescriptor maps the canonical wire contract onto the
// internal transport fields.
func (r *ConnectionDescriptor) NormalizeConnectionDescriptor() error {
	if r.Schema != ConnectionSchemaV1 {
		return errors.New("paperboat-server returned an unsupported connection descriptor schema")
	}
	if r.Environment == nil {
		return nil
	}
	e := r.Environment
	e.EnvironmentID, e.ProjectRoot = e.ID, e.Root
	switch e.Kind {
	case "machine":
		e.UserMachineID, r.UserMachineID, r.UserMachineState = e.ResourceID, e.ResourceID, e.State
	default:
		return errors.New("paperboat-server returned an invalid environment kind")
	}
	if r.Terminal != nil {
		if r.Terminal.Protocol != "paperboat.terminal.v1" {
			return errors.New("invalid canonical terminal protocol")
		}
	}
	if r.FileTransfer != nil {
		u, err := url.Parse(r.FileTransfer.Endpoint)
		if err != nil || u.Scheme == "" || u.Host == "" || strings.TrimRight(u.Path, "/") == "" {
			return errors.New("invalid canonical file transfer endpoint")
		}
		r.FileTransfer.Endpoint = strings.TrimRight(r.FileTransfer.Endpoint, "/")
	}
	return nil
}

// UserMachineConnectionDescriptor obtains the default terminal session's short-lived
// Paperboat descriptor. It deliberately does not accept a client-supplied
// route or connector credential.
func (c *Client) UserMachineConnectionDescriptor(ctx context.Context, machineID string) (ConnectionDescriptor, error) {
	return c.UserMachineConnectionDescriptorForSession(ctx, machineID, "")
}

// UserMachineConnectionDescriptorWithSessionCreate creates a durable terminal
// session and issues the connection descriptor in one round trip. The
// idempotency key makes retried requests resolve the same durable session.
func (c *Client) UserMachineConnectionDescriptorWithSessionCreate(ctx context.Context, machineID, name, idempotencyKey, cwd string) (ConnectionDescriptor, TerminalSession, error) {
	if strings.TrimSpace(machineID) == "" || strings.TrimSpace(idempotencyKey) == "" || c.sourceMachineID == "" {
		return ConnectionDescriptor{}, TerminalSession{}, errors.New("machine identity and idempotency key are required")
	}
	var out struct {
		Descriptor      ConnectionDescriptor `json:"descriptor"`
		TerminalSession TerminalSession      `json:"terminal_session"`
	}
	create := map[string]string{"name": name, "idempotency_key": idempotencyKey}
	if cwd != "" {
		create["cwd"] = cwd
	}
	err := c.do(ctx, http.MethodPost, "/v1/machines/"+url.PathEscape(machineID)+"/connection-descriptor", map[string]any{
		"source_machine_id": c.sourceMachineID,
		"create_session":    create,
	}, &out)
	if err == nil {
		err = out.Descriptor.NormalizeConnectionDescriptor()
	}
	if err == nil && out.TerminalSession.ID == "" {
		err = errors.New("server did not return the created terminal session")
	}
	return out.Descriptor, out.TerminalSession, err
}

// UserMachineConnectionDescriptorForSession connects a durable terminal session belonging
// to an enrolled machine.
func (c *Client) UserMachineConnectionDescriptorForSession(ctx context.Context, machineID, terminalSessionID string) (ConnectionDescriptor, error) {
	var out ConnectionDescriptor
	var values map[string]string
	if c.sourceMachineID != "" {
		values = map[string]string{"source_machine_id": c.sourceMachineID}
	}
	if terminalSessionID != "" {
		if values == nil {
			values = make(map[string]string)
		}
		values["terminal_session_id"] = terminalSessionID
	}
	var body any
	if values != nil {
		body = values
	}
	err := c.do(ctx, http.MethodPost, "/v1/machines/"+url.PathEscape(machineID)+"/connection-descriptor", body, &out)
	if err == nil {
		err = out.NormalizeConnectionDescriptor()
	}
	return out, err
}

func (c *Client) MachineExecDescriptor(ctx context.Context, machineID, operationID string) (ExecDescriptor, error) {
	if strings.TrimSpace(machineID) == "" || len(operationID) < 8 || len(operationID) > 128 || c.sourceMachineID == "" {
		return ExecDescriptor{}, errors.New("machine, source machine, and operation IDs are required")
	}
	var out ExecDescriptor
	err := c.doIssuance(ctx, http.MethodPost, "/v1/machines/"+url.PathEscape(machineID)+"/exec-descriptor", operationDescriptorRequest{SourceMachineID: c.sourceMachineID, OperationID: operationID, NativeOnlyAccepted: true}, &out, false)
	if err == nil {
		err = validateOperationDescriptor(out, machineID, operationID, "exec:operate", "exec")
	}
	return out, err
}

func (c *Client) MachineSSHDescriptor(ctx context.Context, machineID, operationID string) (SSHDescriptor, error) {
	if strings.TrimSpace(machineID) == "" || len(operationID) < 8 || len(operationID) > 128 || c.sourceMachineID == "" {
		return SSHDescriptor{}, errors.New("machine, source machine, and operation IDs are required")
	}
	var out SSHDescriptor
	err := c.doIssuance(ctx, http.MethodPost, "/v1/machines/"+url.PathEscape(machineID)+"/ssh-descriptor", operationDescriptorRequest{SourceMachineID: c.sourceMachineID, OperationID: operationID, NativeOnlyAccepted: true}, &out, false)
	if err == nil {
		err = validateOperationDescriptor(out, machineID, operationID, "ssh:operate", "ssh")
	}
	return out, err
}

// operationDescriptorRequest asks for an exec or SSH descriptor. NativeOnlyAccepted lets the server
// answer while the machine's edge runtime carrier is not current (after a restart, an outage or an edge
// incident): the operation runs over the native peer connection, so the descriptor then has no edge
// endpoints instead of the server refusing it with machine_not_ready.
type operationDescriptorRequest struct {
	SourceMachineID    string `json:"source_machine_id"`
	OperationID        string `json:"operation_id"`
	NativeOnlyAccepted bool   `json:"native_only_accepted"`
}

// validEdgeEndpoints reports whether e is what the server sends: nothing (a native-only descriptor), both edge
// endpoints, or only a wss endpoint when the edge ingress has a path prefix (path-based ingress carries no QUIC
// endpoint, server machineTerminalEndpoints). Half a pair on a root-path ingress is refused.
func validEdgeEndpoints(e TerminalEndpoints) bool {
	if e.QUIC == "" && e.WSS == "" {
		return true
	}
	wss, wssErr := url.Parse(e.WSS)
	if wssErr != nil || wss.Scheme != "wss" || wss.Hostname() == "" || wss.User != nil || wss.RawQuery != "" || wss.Fragment != "" || !strings.HasSuffix(wss.Path, "/v1/runtime") {
		return false
	}
	if e.QUIC == "" {
		return wss.Path != "/v1/runtime"
	}
	quic, quicErr := url.Parse(e.QUIC)
	return quicErr == nil && quic.Scheme == "quic" && quic.Hostname() != "" && quic.User == nil && quic.Path == "" && quic.RawQuery == "" && quic.Fragment == "" && wss.Path == "/v1/runtime"
}

func validateOperationDescriptor(out ExecDescriptor, machineID, operationID, expectedScope, operationKind string) error {
	if out.OperationID != operationID || out.Environment == nil || out.Environment.ID == "" || out.Environment.Kind != "machine" || out.Environment.ResourceID != machineID || out.Environment.State != "ready" || !remotepath.Absolute(out.Environment.Root) ||
		!validEdgeEndpoints(out.Endpoints) ||
		out.Auth.Method != "bearer" || out.Auth.Token == "" || len(out.Auth.Scopes) != 1 || out.Auth.Scopes[0] != expectedScope || out.ExpiresAt.IsZero() || out.Auth.ExpiresAt.IsZero() || !out.ExpiresAt.Equal(out.Auth.ExpiresAt) {
		return fmt.Errorf("invalid %s descriptor", operationKind)
	}
	return nil
}

func (c *Client) MachineFileTransferDescriptor(ctx context.Context, destinationMachineID, sourceMachineID, sessionID string) (FileTransfer, error) {
	return c.MachineFileTransferDescriptorForRequest(ctx, destinationMachineID, sourceMachineID, sessionID, "", "", TransferDeclaration{})
}

// TransferDeclaration is what a send declares for its credential (PLAN 3b File transfer modes): the mode and the
// batch total in bytes. The server signs an automatic credential with max_bytes = Bytes. Zero declares nothing.
type TransferDeclaration struct {
	Mode  string
	Bytes int64
}

func (c *Client) MachineFileTransferDescriptorForRequest(ctx context.Context, destinationMachineID, sourceMachineID, sessionID, requestID, manifestDigest string, declared TransferDeclaration) (FileTransfer, error) {
	if strings.TrimSpace(destinationMachineID) == "" || strings.TrimSpace(sourceMachineID) == "" {
		return FileTransfer{}, errors.New("source and destination machine IDs are required")
	}
	var out FileTransfer
	body := map[string]any{
		"source_machine_id": sourceMachineID,
		"session_id":        sessionID,
	}
	if requestID != "" || manifestDigest != "" {
		body["request_id"], body["manifest_digest"] = requestID, manifestDigest
	}
	if declared.Mode != "" {
		body["transfer_mode"], body["declared_bytes"] = declared.Mode, declared.Bytes
	}
	err := c.do(ctx, http.MethodPost, "/v1/machines/"+url.PathEscape(destinationMachineID)+"/file-transfer-descriptor", body, &out)
	return out, err
}

// UserMachineConnectionReadiness polls readiness without minting a fresh
// descriptor. Reconnects re-run UserMachineConnectionDescriptor after this reports
// ready, matching the machine connection flow.
func (c *Client) UserMachineConnectionReadiness(ctx context.Context, machineID string) (ConnectionDescriptor, error) {
	return c.UserMachineConnectionReadinessForSession(ctx, machineID, "")
}

// UserMachineConnectionReadinessForSession preserves the selected terminal
// session through readiness polling.
func (c *Client) UserMachineConnectionReadinessForSession(ctx context.Context, machineID, terminalSessionID string) (ConnectionDescriptor, error) {
	var out ConnectionDescriptor
	path := "/v1/machines/" + url.PathEscape(machineID) + "/connection-readiness"
	if terminalSessionID != "" {
		path += "?terminal_session_id=" + url.QueryEscape(terminalSessionID)
	}
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	if err == nil {
		err = out.NormalizeConnectionDescriptor()
	}
	return out, err
}

// ListUserMachineTerminalSessions lists the durable Paperboat sessions
// for a machine. Session records remain server-owned, so the CLI
// never discovers local paths or connector state through this endpoint.
func (c *Client) ListUserMachineTerminalSessions(ctx context.Context, machineID string) ([]TerminalSession, error) {
	return c.ListUserMachineTerminalSessionsFiltered(ctx, machineID, nil)
}

// ListUserMachineTerminalSessionsPage reads at most maxItems sessions and the server's total (PQ-04).
func (c *Client) ListUserMachineTerminalSessionsPage(ctx context.Context, machineID string, filters url.Values, maxItems int) ([]TerminalSession, int, error) {
	return collectOffsetInventoryPage[TerminalSession](ctx, c, "/v1/machines/"+url.PathEscape(machineID)+"/terminal-sessions", "items", filters, maxItems)
}

func (c *Client) ListUserMachineTerminalSessionsFiltered(ctx context.Context, machineID string, filters url.Values) ([]TerminalSession, error) {
	return collectOffsetInventory[TerminalSession](ctx, c, "/v1/machines/"+url.PathEscape(machineID)+"/terminal-sessions", "items", filters)
}

func (c *Client) CreateUserMachineTerminalSession(ctx context.Context, machineID, name, idempotencyKey string) (TerminalSession, error) {
	var out TerminalSession
	body := map[string]string{}
	if name != "" {
		body["name"] = name
	}
	path := "/v1/machines/" + url.PathEscape(machineID) + "/terminal-sessions"
	return out, c.doWithHeaders(ctx, http.MethodPost, path, body, &out, http.Header{"Idempotency-Key": []string{idempotencyKey}})
}

func (c *Client) RenameUserMachineTerminalSession(ctx context.Context, machineID, sessionID, name string) (TerminalSession, error) {
	var out TerminalSession
	path := "/v1/machines/" + url.PathEscape(machineID) + "/terminal-sessions/" + url.PathEscape(sessionID)
	err := c.do(ctx, http.MethodPatch, path, map[string]string{"name": name}, &out)
	return out, err
}

type TerminalSessionCloseResult struct {
	OperationState string `json:"operation_state"`
}

func (c *Client) CloseUserMachineTerminalSession(ctx context.Context, machineID, sessionID string) (TerminalSessionCloseResult, error) {
	var out TerminalSessionCloseResult
	path := "/v1/machines/" + url.PathEscape(machineID) + "/terminal-sessions/" + url.PathEscape(sessionID) + "/close"
	if err := c.do(ctx, http.MethodPost, path, nil, &out); err != nil {
		return out, err
	}
	if out.OperationState != "pending" && out.OperationState != "applied" {
		return out, &ResponseDecodeError{Err: errors.New("invalid terminal close operation state")}
	}
	return out, nil
}

func (c *Client) DeleteUserMachineTerminalSession(ctx context.Context, machineID, sessionID string) error {
	path := "/v1/machines/" + url.PathEscape(machineID) + "/terminal-sessions/" + url.PathEscape(sessionID)
	return c.do(ctx, http.MethodDelete, path, nil, &struct{}{})
}

// FileTransferMode is the account's default transfer mode, "automatic" or "unlimited" (GET /v1/me/preferences).
func (c *Client) FileTransferMode(ctx context.Context) (string, error) {
	var out struct {
		FileTransferMode string `json:"file_transfer_mode"`
	}
	if err := c.do(ctx, http.MethodGet, "/v1/me/preferences", nil, &out); err != nil {
		return "", err
	}
	if out.FileTransferMode != "automatic" && out.FileTransferMode != "unlimited" {
		return "", errors.New("server returned an unknown file transfer mode")
	}
	return out.FileTransferMode, nil
}
