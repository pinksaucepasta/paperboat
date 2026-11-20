package api

import (
	"context"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/shared/terminalprotocol"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	clientapi "github.com/pinksaucepasta/paperboat/shared/client/api"
	"github.com/pinksaucepasta/paperboat/shared/remotepath"
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
	Columns           *int             `json:"columns,omitempty"`
	Rows              *int             `json:"rows,omitempty"`
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
type AuthMaterial = terminalprotocol.Auth

const ConnectionSchemaV1 = terminalprotocol.DescriptorSchemaV1

// Environment identifies the enrolled machine behind a terminal descriptor.
type Environment = terminalprotocol.Environment

// Terminal is the CLI-safe Paperboat WebSocket attach descriptor from
// cli-connect. It carries client-safe Paperboat route URLs, not raw VM
// addresses or SSH credentials.
type Terminal = terminalprotocol.Terminal

type TerminalEndpoints = terminalprotocol.TerminalEndpoints

type FileTransferPolicy = terminalprotocol.FileTransferPolicy

const AutomaticTransferLimit = terminalprotocol.AutomaticTransferLimit

type FileTransfer = terminalprotocol.FileTransfer

// ConnectionDescriptor is the cli-connect / connection-status descriptor. When
// Connectable is false the machine is not ready yet; Status/Reason explain why
// and the caller should poll ConnectionReadiness.
type ConnectionDescriptor = terminalprotocol.Descriptor

type ExecDescriptor = terminalprotocol.Operation

type SSHDescriptor = ExecDescriptor

// NormalizeConnectionDescriptor checks the descriptor's schema and the parts the client acts on: the environment kind,
// the terminal protocol, and a well-formed file-transfer endpoint (trimmed of a trailing slash).
func NormalizeConnectionDescriptor(r *ConnectionDescriptor) error {
	if r.Schema != ConnectionSchemaV1 {
		return errors.New("paperboat-server returned an unsupported connection descriptor schema")
	}
	if r.Environment.ID == "" {
		return nil
	}
	if r.Environment.Kind != terminalprotocol.EnvironmentMachine {
		return errors.New("paperboat-server returned an invalid environment kind")
	}
	if r.Terminal != nil && r.Terminal.Protocol != terminalprotocol.TerminalSubprotocol {
		return errors.New("invalid canonical terminal protocol")
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
	// A 503 under a burst of opens (F21-164: 1 in 100 printed "temporarily unavailable") is asked again: the
	// idempotency key resolves a repeat to the same session.
	err := c.doIssuance(ctx, http.MethodPost, "/v1/machines/"+url.PathEscape(machineID)+"/connection-descriptor", map[string]any{
		"source_machine_id": c.sourceMachineID,
		"create_session":    create,
	}, &out, false)
	if err == nil {
		err = NormalizeConnectionDescriptor(&out.Descriptor)
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
	// A busy 503 is asked again (F21-164: 9 of 50 viewers joining one session at once exited "temporarily
	// unavailable"); a repeat only issues the connection again.
	err := c.doIssuance(ctx, http.MethodPost, "/v1/machines/"+url.PathEscape(machineID)+"/connection-descriptor", body, &out, false)
	if err == nil {
		err = NormalizeConnectionDescriptor(&out)
	}
	return out, err
}

func (c *Client) MachineExecDescriptor(ctx context.Context, machineID, operationID string) (ExecDescriptor, error) {
	return c.machineOperationDescriptor(ctx, "exec-descriptor", "exec:operate", "exec", machineID, operationID)
}

// MachineSSHDescriptor is a managed SSH operation credential, for a session the server authorizes per operation (an
// API token or recipe sign-in): hosts hold no standing SSH rule for it.
func (c *Client) MachineSSHDescriptor(ctx context.Context, machineID, operationID string) (SSHDescriptor, error) {
	return c.machineOperationDescriptor(ctx, "ssh-descriptor", "ssh:operate", "ssh", machineID, operationID)
}

func (c *Client) machineOperationDescriptor(ctx context.Context, route, scope, kind, machineID, operationID string) (ExecDescriptor, error) {
	if strings.TrimSpace(machineID) == "" || len(operationID) < 8 || len(operationID) > 128 || c.sourceMachineID == "" {
		return ExecDescriptor{}, errors.New("machine, source machine, and operation IDs are required")
	}
	var out ExecDescriptor
	err := c.doIssuance(ctx, http.MethodPost, "/v1/machines/"+url.PathEscape(machineID)+"/"+route, operationDescriptorRequest{SourceMachineID: c.sourceMachineID, OperationID: operationID, NativeOnlyAccepted: true}, &out, false)
	if err == nil {
		err = validateOperationDescriptor(out, machineID, operationID, scope, kind)
	}
	return out, err
}

// operationDescriptorRequest asks for an exec descriptor. NativeOnlyAccepted lets the server
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
	if out.OperationID != operationID || out.Environment.ID == "" || out.Environment.Kind != "machine" || out.Environment.ResourceID != machineID || out.Environment.State != "ready" || !remotepath.Absolute(out.Environment.Root) ||
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

// ReleaseFileTransferSessions ends a send's credentials after its streams close.
func (c *Client) ReleaseFileTransferSessions(ctx context.Context, ids []string) error {
	for batch := range slices.Chunk(ids, 64) {
		if err := c.do(ctx, http.MethodPost, "/v1/file-transfer-sessions/release", map[string][]string{"access_session_ids": batch}, nil); err != nil {
			return err
		}
	}
	return nil
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
		err = NormalizeConnectionDescriptor(&out)
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
	return clientapi.CollectOffsetInventoryPage[TerminalSession](ctx, c.retryingInventoryRequest(), "/v1/machines/"+url.PathEscape(machineID)+"/terminal-sessions", "items", filters, maxItems)
}

func (c *Client) ListUserMachineTerminalSessionsFiltered(ctx context.Context, machineID string, filters url.Values) ([]TerminalSession, error) {
	return clientapi.CollectOffsetInventory[TerminalSession](ctx, c.retryingInventoryRequest(), "/v1/machines/"+url.PathEscape(machineID)+"/terminal-sessions", "items", filters)
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

// AdoptOfflineTerminalSession registers a session the machine started while Paperboat could not be reached
// (terminal_offline_*) once the machine is online; it is then listed like every other session.
func (c *Client) AdoptOfflineTerminalSession(ctx context.Context, machineID, sessionID string) (TerminalSession, error) {
	var out TerminalSession
	path := "/v1/machines/" + url.PathEscape(machineID) + "/terminal-sessions/" + url.PathEscape(sessionID) + "/adopt"
	err := c.do(ctx, http.MethodPost, path, nil, &out)
	return out, err
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
