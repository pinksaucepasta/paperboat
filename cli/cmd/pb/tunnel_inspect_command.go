package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
	"github.com/spf13/cobra"
)

// inspectorDaemonMaxBytes bounds every daemon-local inspector response. Pages
// are already bounded server-side (100 records/1 MiB); this is a final guard.
const inspectorDaemonMaxBytes = 2 << 20

var errInspectorDaemonUnavailable = errors.New("remote inspector unavailable: check the owner daemon and your native connection")

// inspectorGrantError reports a denied grant without exposing token material.
var errInspectorGrantDenied = errors.New("inspector access denied for this account and action: request an explicit inspect or replay grant for the exact resource")

type inspectorCaller interface {
	do(context.Context, string, string, string, any, url.Values) ([]byte, int, error)
}

type inspectorNativeClient struct {
	peer   *tunnel.PeerTerminalTunnel
	access api.InspectorAccess
}

func (c *inspectorNativeClient) do(ctx context.Context, grant, method, path string, body any, query url.Values) ([]byte, int, error) {
	var payload []byte
	var err error
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
	}
	return c.peer.InspectorRequest(ctx, c.access, grant, method, path, payload, query)
}

// inspectorDaemonError preserves the daemon's stable error code so callers
// branch on Code instead of parsing message text.
type inspectorDaemonError struct {
	Status int
	Code   string
	Reason string
}

func (e *inspectorDaemonError) Error() string {
	if e == nil {
		return "inspector request failed"
	}
	switch e.Code {
	case "inspector_authority_stale":
		return "inspector authority changed; list captures again and retry"
	case "inspector_generation_stale":
		return "resource changed since capture; list captures again and retry"
	case "inspector_not_found", "inspector_expired", "inspector_not_enabled":
		return "capture is unavailable (expired, purged, disabled or never captured)"
	case "inspector_replay_ineligible":
		if e.Reason != "" {
			return "capture cannot be replayed: " + e.Reason
		}
		return "capture cannot be replayed"
	case "inspector_replay_conflict":
		return "idempotency key was already used for a different replay; use a fresh key"
	case "inspector_replay_ambiguous":
		return "replay result is ambiguous: the origin may have received the request; do not retry with a new key, reuse the same idempotency key to recover the outcome"
	case "inspector_busy":
		return "daemon is busy with other replays; retry shortly"
	case "inspector_audit_unavailable", "inspector_over_budget":
		return "daemon cannot record the replay right now; retry shortly"
	case "inspector_forbidden", "inspector_unauthorized":
		return errInspectorGrantDenied.Error()
	default:
		return "inspector request failed: " + e.Code
	}
}

func decodeInspectorDaemonError(status int, payload []byte) error {
	var envelope struct {
		Code   string `json:"code"`
		Reason string `json:"reason"`
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if decoder.Decode(&envelope) != nil || strings.TrimSpace(envelope.Code) == "" {
		return errInspectorDaemonUnavailable
	}
	return &inspectorDaemonError{Status: status, Code: envelope.Code, Reason: envelope.Reason}
}

type inspectorCapture struct {
	ID                string              `json:"id"`
	ResourceID        string              `json:"resource_id"`
	ResourceGen       uint64              `json:"resource_generation"`
	RouteGen          uint64              `json:"route_generation"`
	TargetGen         uint64              `json:"target_generation"`
	Method            string              `json:"method"`
	URL               string              `json:"url"`
	URLTruncated      bool                `json:"url_truncated"`
	RequestHeaders    map[string][]string `json:"request_headers"`
	ResponseHeaders   map[string][]string `json:"response_headers"`
	HeadersTruncated  bool                `json:"headers_truncated"`
	RequestBody       string              `json:"request_body"`
	RequestBodyState  string              `json:"request_body_state"`
	ResponseBody      string              `json:"response_body"`
	ResponseBodyState string              `json:"response_body_state"`
	ResponseStatus    int                 `json:"response_status"`
	ErrorCode         string              `json:"error_code"`
	State             string              `json:"state"`
	ReplayIneligible  string              `json:"replay_ineligible"`
	StartedAt         time.Time           `json:"started_at"`
	FinishedAt        time.Time           `json:"finished_at"`
}

type inspectorReplayResult struct {
	IdempotencyKey    string    `json:"idempotency_key,omitempty"`
	OperationID       string    `json:"operation_id"`
	CaptureID         string    `json:"capture_id"`
	ResourceID        string    `json:"resource_id"`
	Method            string    `json:"method"`
	URL               string    `json:"url"`
	RequestBodyState  string    `json:"request_body_state"`
	ResponseStatus    int       `json:"response_status"`
	ResponseBodyState string    `json:"response_body_state"`
	ReplayRecordID    string    `json:"replay_record_id"`
	SideEffects       string    `json:"side_effects"`
	StartedAt         time.Time `json:"started_at"`
	FinishedAt        time.Time `json:"finished_at"`
	ErrorCode         string    `json:"error_code"`
	Code              string    `json:"code"`
}

func decodeInspectorPayload(payload []byte, out any) error {
	// Tolerant decode: the daemon envelope carries schema/kind/resource
	// fields around the shapes below, and must stay forward-compatible.
	decoder := json.NewDecoder(bytes.NewReader(payload))
	if err := decoder.Decode(out); err != nil {
		return errInspectorDaemonUnavailable
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errInspectorDaemonUnavailable
	}
	return nil
}

func validInspectorResourceID(value string) bool {
	return len(value) >= 1 && len(value) <= 256 && value == strings.TrimSpace(value) && !strings.ContainsAny(value, "\x00\r\n\t /\\?#%")
}

// inspectorIssueCredential is a seam for deterministic command tests: it
// issues the caller's own server-minted grant for one exact action.
var inspectorIssueCredential = func(ctx context.Context, client *api.Client, kind, resource, route, action string) (api.InspectorCredential, error) {
	return client.IssueInspectorCredential(ctx, kind, resource, route, action)
}

type inspectorClients struct {
	server  *api.Client
	daemon  inspectorCaller
	prepare func(context.Context, api.InspectorCredential, inspectorTarget, string) error
	close   func() error
	ctx     context.Context
}

var inspectorClientsForCommand = productionInspectorClients

func productionInspectorClients(command *cobra.Command) (*inspectorClients, error) {
	d, err := buildDeps(actionContext(command, nil))
	if err != nil {
		return nil, err
	}
	peer := d.hostedTransferKeys
	if peer == nil {
		return nil, errInspectorDaemonUnavailable
	}
	credential, err := d.auth.Credential()
	if err != nil {
		_ = peer.Close()
		return nil, err
	}
	server := api.New(d.cfg.ServerURL, credential, nil)
	remote := &inspectorNativeClient{peer: peer}
	return &inspectorClients{server: server, daemon: remote, ctx: command.Context(), close: peer.Close,
		prepare: func(ctx context.Context, grant api.InspectorCredential, target inspectorTarget, action string) error {
			access, err := server.ResolveInspectorAccess(ctx, grant.Token, target.kind, target.resource, target.route, action, "native")
			if err == nil {
				remote.access = access
			}
			return err
		}}, nil
}

// inspectorTarget is one exact credential scope: kind plus resource and route
// IDs as the server binds them (preview leases use the lease ID twice).
type inspectorTarget struct {
	kind, resource, route string
}

func resolveTunnelInspectorTarget(ctx context.Context, client *api.Client, tunnelSelector, routeFlag, action string) (inspectorTarget, error) {
	if strings.HasPrefix(tunnelSelector, "tun_") && validInspectorResourceID(tunnelSelector) && routeFlag != "" {
		if !validInspectorResourceID(routeFlag) {
			return inspectorTarget{}, errors.New("route argument is invalid")
		}
		return inspectorTarget{kind: "tunnel", resource: tunnelSelector, route: routeFlag}, nil
	}
	targets, err := client.ResolveInspectorTargets(ctx, tunnelSelector, action)
	if err != nil {
		return inspectorTarget{}, err
	}
	resources := map[string]bool{}
	selected := []api.InspectorTarget{}
	for _, target := range targets {
		resources[target.ResourceID] = true
		if routeFlag == "" || target.RouteID == routeFlag {
			selected = append(selected, target)
		}
	}
	if len(resources) > 1 {
		return inspectorTarget{}, errors.New("tunnel name is ambiguous; use a canonical tunnel ID")
	}
	if len(selected) == 0 {
		return inspectorTarget{}, errors.New("tunnel has no matching route authorized for this inspector action")
	}
	if len(selected) > 1 {
		ids := make([]string, 0, len(selected))
		for _, target := range selected {
			ids = append(ids, target.RouteID)
		}
		return inspectorTarget{}, fmt.Errorf("tunnel has %d routes; retry with --route set to one of: %s", len(selected), strings.Join(ids, ", "))
	}
	return inspectorTarget{kind: "tunnel", resource: selected[0].ResourceID, route: selected[0].RouteID}, nil
}

// withInspectorGrant issues the caller's own grant for one action, runs the
// daemon operation, and retries exactly once with a fresh issuance when the
// daemon reports a denial (covering generation advances between issuance and
// use). A second denial surfaces; transport failures never trigger issuance.
func withInspectorGrant(clients *inspectorClients, target inspectorTarget, action string, run func(ctx context.Context, grant string) (bool, error)) error {
	if clients == nil {
		return errInspectorGrantDenied
	}
	for attempt := 0; attempt < 2; attempt++ {
		grant, err := inspectorIssueCredential(clients.ctx, clients.server, target.kind, target.resource, target.route, action)
		if err != nil {
			return err
		}
		retry, runErr := func() (bool, error) {
			defer func() {
				cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(clients.ctx), 5*time.Second)
				defer cancel()
				_ = clients.server.RevokeInspectorCredential(cleanupCtx, grant.CredentialID)
			}()
			if clients.prepare != nil {
				if err := clients.prepare(clients.ctx, grant, target, action); err != nil {
					return false, err
				}
			}
			return run(clients.ctx, grant.Token)
		}()
		if runErr == nil || !retry || attempt == 1 {
			return runErr
		}
	}
	return errInspectorGrantDenied
}

// daemonDenied reports whether a daemon call failed with a denial worth one
// fresh-issuance retry (as opposed to a terminal or transport failure).
func daemonDenied(status int, payload []byte) bool {
	return status == http.StatusForbidden
}

func tunnelInspectCommand() *cobra.Command {
	command := tunnelCommand("inspect <tunnel>", "Show daemon-local HTTP captures", cobra.ExactArgs(1), runTunnelInspect)
	command.Flags().String("route", "", "inspect one route ID (required when the tunnel has several routes)")
	command.Flags().String("cursor", "", "continue after a capture cursor")
	command.Flags().Int("limit", 20, "maximum captures per request (1-100)")
	command.Flags().String("id", "", "show one capture by ID")
	command.Flags().Bool("enable", false, "enable capture for this resource")
	command.Flags().Bool("disable", false, "disable capture for this resource")
	command.Flags().Bool("body", false, "capture redacted request/response bodies with --enable")
	command.Flags().Bool("raw", false, "retain exact raw requests for replay with --enable")
	command.Flags().Bool("purge", false, "purge retained captures for this resource")
	tunnelJSONFlag(command)
	return command
}

func previewInspectCommand() *cobra.Command {
	command := tunnelCommand("inspect <preview>", "Show daemon-local HTTP captures", cobra.ExactArgs(1), runPreviewInspect)
	command.Flags().String("cursor", "", "continue after a capture cursor")
	command.Flags().Int("limit", 20, "maximum captures per request (1-100)")
	command.Flags().String("id", "", "show one capture by ID")
	command.Flags().Bool("enable", false, "enable capture for this resource")
	command.Flags().Bool("disable", false, "disable capture for this resource")
	command.Flags().Bool("body", false, "capture redacted request/response bodies with --enable")
	command.Flags().Bool("raw", false, "retain exact raw requests for replay with --enable")
	command.Flags().Bool("purge", false, "purge retained captures for this resource")
	tunnelJSONFlag(command)
	return command
}

type tunnelInspectOptions struct {
	cursor    string
	limit     int
	captureID string
	enable    bool
	disable   bool
	withBody  bool
	withRaw   bool
	purge     bool
}

func readTunnelInspectOptions(command *cobra.Command) (tunnelInspectOptions, error) {
	options := tunnelInspectOptions{}
	var err error
	if options.cursor, _ = command.Flags().GetString("cursor"); len(options.cursor) > 256 || strings.ContainsAny(options.cursor, "\x00\r\n") {
		return options, errors.New("cursor is invalid")
	}
	if options.limit, _ = command.Flags().GetInt("limit"); options.limit < 1 || options.limit > 100 {
		return options, errors.New("limit must be between 1 and 100")
	}
	if options.captureID, _ = command.Flags().GetString("id"); len(options.captureID) > 256 || strings.ContainsAny(options.captureID, "\x00\r\n/") {
		return options, errors.New("id is invalid")
	}
	options.enable, _ = command.Flags().GetBool("enable")
	options.disable, _ = command.Flags().GetBool("disable")
	options.withBody, _ = command.Flags().GetBool("body")
	options.withRaw, _ = command.Flags().GetBool("raw")
	options.purge, _ = command.Flags().GetBool("purge")
	if (options.withBody || options.withRaw) && !options.enable {
		return options, errors.New("--body and --raw require --enable")
	}
	selected := 0
	for _, active := range []bool{options.purge, options.enable, options.disable, options.captureID != ""} {
		if active {
			selected++
		}
	}
	if selected > 1 {
		return options, errors.New("--purge, --enable/--disable and --id are mutually exclusive")
	}
	return options, err
}

func runTunnelInspect(command *cobra.Command, args []string) error {
	if !validInspectorResourceID(args[0]) {
		return errors.New("tunnel argument is invalid")
	}
	options, err := readTunnelInspectOptions(command)
	if err != nil {
		return err
	}
	clients, err := inspectorClientsForCommand(command)
	if err != nil {
		return err
	}
	if clients.close != nil {
		defer clients.close()
	}
	routeFlag, _ := command.Flags().GetString("route")
	action := "inspect"
	if options.enable || options.disable || options.purge {
		action = "replay"
	}
	target, err := resolveTunnelInspectorTarget(clients.ctx, clients.server, args[0], routeFlag, action)
	if err != nil {
		return err
	}
	return runInspectorView(command, clients, target, options, args[0])
}

func runPreviewInspect(command *cobra.Command, args []string) error {
	if !validInspectorResourceID(args[0]) {
		return errors.New("preview argument is invalid")
	}
	options, err := readTunnelInspectOptions(command)
	if err != nil {
		return err
	}
	clients, err := inspectorClientsForCommand(command)
	if err != nil {
		return err
	}
	if clients.close != nil {
		defer clients.close()
	}
	return runInspectorView(command, clients, inspectorTarget{kind: "preview", resource: args[0], route: args[0]}, options, args[0])
}

func runInspectorView(command *cobra.Command, clients *inspectorClients, target inspectorTarget, options tunnelInspectOptions, display string) error {
	action := "inspect"
	if options.enable || options.disable || options.purge {
		action = "replay"
	}
	return withInspectorGrant(clients, target, action, func(ctx context.Context, grant string) (bool, error) {
		switch {
		case options.purge:
			payload, status, err := clients.daemon.do(ctx, grant, http.MethodDelete, "/v1/inspector/records", nil, url.Values{"kind": {target.kind}, "resource": {target.resource}, "route": {target.route}})
			if err != nil {
				return false, err
			}
			if status != http.StatusOK {
				return daemonDenied(status, payload), decodeInspectorDaemonError(status, payload)
			}
			return false, tunnelOutput(command, map[string]string{"resource_id": display, "purged": "true"}, "Purged inspector captures for "+display)
		case options.enable || options.disable:
			payload, status, err := clients.daemon.do(ctx, grant, http.MethodPut, "/v1/inspector/policy", map[string]any{
				"resource_kind": target.kind, "resource_id": target.resource, "route_id": target.route, "enabled": options.enable,
				"capture_request_body": options.withBody, "capture_response_body": options.withBody, "capture_raw": options.withRaw,
			}, nil)
			if err != nil {
				return false, err
			}
			if status != http.StatusOK {
				return daemonDenied(status, payload), decodeInspectorDaemonError(status, payload)
			}
			state := "disabled"
			if options.enable {
				state = "enabled"
			}
			return false, tunnelOutput(command, map[string]string{"resource_id": display, "capture": state}, "Inspector capture "+state+" for "+display)
		case options.captureID != "":
			payload, status, err := clients.daemon.do(ctx, grant, http.MethodGet, "/v1/inspector/records/"+options.captureID, nil, url.Values{"kind": {target.kind}, "resource": {target.resource}, "route": {target.route}})
			if err != nil {
				return false, err
			}
			if status != http.StatusOK {
				return daemonDenied(status, payload), decodeInspectorDaemonError(status, payload)
			}
			var record inspectorCapture
			if err := decodeInspectorPayload(payload, &record); err != nil {
				return false, err
			}
			return false, tunnelOutput(command, record, fmt.Sprintf("%s\t%s\t%d\t%s", record.ID, record.Method, record.ResponseStatus, record.State))
		default:
			payload, status, err := clients.daemon.do(ctx, grant, http.MethodGet, "/v1/inspector/records", nil, url.Values{"kind": {target.kind}, "resource": {target.resource}, "route": {target.route}, "cursor": {options.cursor}, "limit": {strconv.Itoa(options.limit)}})
			if err != nil {
				return false, err
			}
			if status != http.StatusOK {
				return daemonDenied(status, payload), decodeInspectorDaemonError(status, payload)
			}
			var page struct {
				Records    []inspectorCapture `json:"records"`
				NextCursor string             `json:"next_cursor"`
			}
			if err := decodeInspectorPayload(payload, &page); err != nil {
				return false, err
			}
			if jsonOutput, _ := command.Flags().GetBool("json"); jsonOutput {
				return false, json.NewEncoder(command.OutOrStdout()).Encode(page)
			}
			writer := command.OutOrStdout()
			if len(page.Records) == 0 {
				_, err := fmt.Fprintln(writer, "No captures retained for "+display+". Enable capture first.")
				return false, err
			}
			for _, record := range page.Records {
				if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%d\t%s\n", record.ID, record.Method, record.State, record.ResponseStatus, record.URL); err != nil {
					return false, err
				}
			}
			if page.NextCursor != "" {
				_, err := fmt.Fprintf(writer, "Next cursor: %s\n", page.NextCursor)
				return false, err
			}
			return false, nil
		}
	})
}

func tunnelReplayCommand() *cobra.Command {
	command := tunnelCommand("replay <tunnel> <capture-id>", "Deliberately replay one retained HTTP request to the same origin", cobra.ExactArgs(2), runTunnelReplay)
	command.Flags().String("route", "", "replay a capture from one route ID (required when the tunnel has several routes)")
	command.Flags().String("idempotency-key", "", "explicit idempotency key for safe retry (generated when omitted)")
	tunnelJSONFlag(command)
	return command
}

func previewReplayCommand() *cobra.Command {
	command := tunnelCommand("replay <preview> <capture-id>", "Deliberately replay one retained HTTP request to the same origin", cobra.ExactArgs(2), runPreviewReplay)
	command.Flags().String("idempotency-key", "", "explicit idempotency key for safe retry (generated when omitted)")
	tunnelJSONFlag(command)
	return command
}

func runTunnelReplay(command *cobra.Command, args []string) error {
	if !validInspectorResourceID(args[0]) {
		return errors.New("tunnel argument is invalid")
	}
	clients, err := inspectorClientsForCommand(command)
	if err != nil {
		return err
	}
	if clients.close != nil {
		defer clients.close()
	}
	routeFlag, _ := command.Flags().GetString("route")
	target, err := resolveTunnelInspectorTarget(clients.ctx, clients.server, args[0], routeFlag, "replay")
	if err != nil {
		return err
	}
	return runInspectorReplay(command, clients, target, args[1])
}

func runPreviewReplay(command *cobra.Command, args []string) error {
	if !validInspectorResourceID(args[0]) {
		return errors.New("preview argument is invalid")
	}
	clients, err := inspectorClientsForCommand(command)
	if err != nil {
		return err
	}
	if clients.close != nil {
		defer clients.close()
	}
	return runInspectorReplay(command, clients, inspectorTarget{kind: "preview", resource: args[0], route: args[0]}, args[1])
}

func runInspectorReplay(command *cobra.Command, clients *inspectorClients, target inspectorTarget, captureID string) error {
	if strings.TrimSpace(captureID) == "" || len(captureID) > 256 || strings.ContainsAny(captureID, "\x00\r\n/") {
		return errors.New("capture argument is invalid")
	}
	key, _ := command.Flags().GetString("idempotency-key")
	if key == "" {
		generated, err := newTunnelIdempotencyKey()
		if err != nil {
			return err
		}
		key = "pb_replay_" + strings.TrimPrefix(generated, "pb_tunnel_")
	}
	if len(key) > 256 || strings.TrimSpace(key) == "" {
		return errors.New("idempotency key is invalid")
	}
	return withInspectorGrant(clients, target, "replay", func(ctx context.Context, grant string) (bool, error) {
		payload, status, err := clients.daemon.do(ctx, grant, http.MethodPost, "/v1/inspector/replay", map[string]any{
			"resource_kind": target.kind, "resource_id": target.resource, "route_id": target.route, "capture_id": captureID, "idempotency_key": key,
		}, nil)
		if err != nil {
			_ = writeTunnelReplayResult(command, inspectorReplayResult{SideEffects: "unknown", ErrorCode: "transport_unavailable"}, key)
			return false, fmt.Errorf("replay outcome is unknown; recover with --idempotency-key %s: %w", key, err)
		}
		var result inspectorReplayResult
		if err := decodeInspectorPayload(payload, &result); err != nil {
			if status != http.StatusOK {
				return daemonDenied(status, payload), decodeInspectorDaemonError(status, payload)
			}
			_ = writeTunnelReplayResult(command, inspectorReplayResult{SideEffects: "unknown", ErrorCode: "invalid_response"}, key)
			return false, fmt.Errorf("replay outcome is unknown; recover with --idempotency-key %s: %w", key, err)
		}
		if status != http.StatusOK {
			daemonErr := decodeInspectorDaemonError(status, payload)
			// Ambiguous and other replay outcomes still carry the safe result:
			// show it before reporting the failure so the operator keeps the
			// operation ID for keyed recovery instead of blindly retrying.
			_ = writeTunnelReplayResult(command, result, key)
			return daemonDenied(status, payload), daemonErr
		}
		return false, writeTunnelReplayResult(command, result, key)
	})
}

func writeTunnelReplayResult(command *cobra.Command, result inspectorReplayResult, key string) error {
	result.IdempotencyKey = key
	if jsonOutput, _ := command.Flags().GetBool("json"); jsonOutput {
		return json.NewEncoder(command.OutOrStdout()).Encode(result)
	}
	summary := "Replay " + result.OperationID + " -> origin responded " + strconv.Itoa(result.ResponseStatus)
	if result.ResponseStatus == 0 {
		summary = "Replay outcome is unknown"
	}
	lines := []string{
		summary,
		"Request: " + result.Method + " (retained bytes, same origin only)",
		"Side effects: " + result.SideEffects,
		"Idempotency key: " + key + " (reuse it to recover this outcome; never retry with a new key after an ambiguous result)",
	}
	if result.ReplayRecordID != "" {
		lines = append(lines, "Replay response stored as "+result.ReplayRecordID)
	} else {
		lines = append(lines, "Replay response was not stored (budget or disabled capture)")
	}
	_, err := fmt.Fprintln(command.OutOrStdout(), strings.Join(lines, "\n"))
	return err
}
