package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/tunnelcreatejournal"
	"github.com/spf13/cobra"
)

var tunnelClientForCommand = productionTunnelClientForCommand

var resolveTunnelSelectorForCommand = resolveTunnelSelector

var beginTunnelCreateWorkflowForCommand = beginProductionTunnelCreateWorkflow

var newTunnelIdempotencyKey = func() (string, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}
	return "operation_" + id.String(), nil
}

const (
	tunnelOperationWaitDefault     = 2 * time.Minute
	tunnelOperationWaitMaximum     = 10 * time.Minute
	tunnelDoctorBundleMinBytes     = 64 << 10
	tunnelDoctorBundleMaxBytes     = 8 << 20
	tunnelDoctorBundleDefaultBytes = 2 << 20
	tunnelRequestRetryAttempts     = 3
	tunnelRequestRetryBaseDelay    = 100 * time.Millisecond
	tunnelRequestRetryMaximumDelay = 2 * time.Second
)

var tunnelOperationPollInterval = 500 * time.Millisecond

// tunnelRequestRetryDelay is a seam for deterministic command tests. Retries
// are deliberately short and bounded because commands must remain responsive
// while still recovering ordinary transient control-plane failures.
var tunnelRequestRetryDelay = func(attempt int) time.Duration {
	delay := tunnelRequestRetryBaseDelay
	for i := 0; i < attempt; i++ {
		if delay >= tunnelRequestRetryMaximumDelay/2 {
			delay = tunnelRequestRetryMaximumDelay
			break
		}
		delay *= 2
	}
	if delay > tunnelRequestRetryMaximumDelay {
		delay = tunnelRequestRetryMaximumDelay
	}
	// Full jitter avoids synchronized retries when several CLI processes
	// recover from the same control-plane outage.
	var value [8]byte
	if _, err := rand.Read(value[:]); err == nil {
		var random uint64
		for _, b := range value {
			random = random<<8 | uint64(b)
		}
		return time.Duration(random % uint64(delay+1))
	}
	return delay
}

func tunnelRequestRetryable(err error) bool {
	if err == nil {
		return false
	}
	var apiErr *api.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Retryable()
	}
	var networkErr net.Error
	return errors.As(err, &networkErr) && networkErr.Timeout()
}

func retryTunnelRequest(ctx context.Context, request func() error) error {
	if ctx == nil {
		return context.Canceled
	}
	for attempt := 0; attempt < tunnelRequestRetryAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := request()
		if err == nil || !tunnelRequestRetryable(err) || attempt == tunnelRequestRetryAttempts-1 {
			return err
		}
		timer := time.NewTimer(tunnelRequestRetryDelay(attempt))
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
	return nil
}

func retryTunnelRead[T any](ctx context.Context, read func() (T, error)) (T, error) {
	var value T
	err := retryTunnelRequest(ctx, func() error {
		var readErr error
		value, readErr = read()
		return readErr
	})
	return value, err
}

var tunnelDoctorLocalReportForCommand = collectTunnelDoctorLocalReport

var tunnelDoctorHostDiagnosticsForCommand = collectTunnelHostDiagnostics

var tunnelDoctorDNSCheckForCommand = func(ctx context.Context) bool {
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, "localhost")
	return err == nil && len(addresses) > 0
}

type tunnelCreateWorkflow interface {
	Snapshot() tunnelcreatejournal.Journal
	RecordTunnel(context.Context, string, string) error
	RecordConnectorReady(context.Context) error
	RecordDomain(context.Context, int, string) error
	Complete(context.Context) error
	Close() error
}

type tunnelCreateWorkflowRequest struct {
	Name          string
	RequestDigest string
	DomainCount   int
	ExpiresAt     *time.Time
}

func beginProductionTunnelCreateWorkflow(ctx context.Context, request tunnelCreateWorkflowRequest) (tunnelCreateWorkflow, error) {
	stateRoot, err := previewRuntimeStateRoot()
	if err != nil {
		return nil, err
	}
	store, err := runtimeIdentityStore()
	if err != nil {
		return nil, err
	}
	registration, err := store.Registration()
	if err != nil || !validTunnelCLIResourceID(registration.MachineID) {
		return nil, errors.Join(tunnelcreatejournal.ErrInvalid, err)
	}
	nameDigest := sha256.Sum256([]byte(request.Name))
	return tunnelcreatejournal.Begin(ctx, tunnelcreatejournal.Config{
		StateRoot:     stateRoot,
		HostID:        registration.MachineID,
		NameDigest:    hex.EncodeToString(nameDigest[:]),
		RequestDigest: request.RequestDigest,
		DomainCount:   request.DomainCount,
		ExpiresAt:     request.ExpiresAt,
		NewKey:        tunnelKey,
	})
}

// TunnelOperationOutcomeError preserves the typed terminal operation so
// callers can distinguish a committed failure from a request failure.
type TunnelOperationOutcomeError struct {
	Operation api.TunnelOperation
}

func (e *TunnelOperationOutcomeError) Error() string {
	if e == nil {
		return "tunnel operation failed"
	}
	message := fmt.Sprintf("tunnel operation ended in state %s during %s", e.Operation.State, e.Operation.Phase)
	if e.Operation.Error != nil && e.Operation.Error.RepairAction != "" {
		message += "; repair: " + e.Operation.Error.RepairAction
	}
	return message
}

// TunnelOperationWaitTimeoutError reports an observation timeout without
// implying that the already-committed server operation was canceled.
type TunnelOperationWaitTimeoutError struct {
	OperationID string
	Timeout     time.Duration
}

func (e *TunnelOperationWaitTimeoutError) Error() string {
	return fmt.Sprintf("timed out after %s waiting for tunnel operation %s; the operation remains active and can be inspected with `pb tunnel status <tunnel>`", e.Timeout, e.OperationID)
}

// TunnelCreateChangedError means durable tunnel state exists and must not be
// silently recreated or rolled back after a later local/domain step failed.
type TunnelCreateChangedError struct {
	TunnelID         string
	Stage            string
	Outcome          string
	OutcomeUncertain bool
	RecoveryCommand  string
	Cause            error
}

// TunnelCreateExistingError reports the exact durable tunnel that already
// owns a requested name. A process-restart retry uses a fresh idempotency key,
// so the control plane correctly returns a name conflict instead of silently
// treating an unrelated request as an idempotent replay. The CLI resolves the
// account-scoped resource and gives a safe inspection command without
// attaching this host to it implicitly.
type TunnelCreateExistingError struct {
	TunnelID        string
	Name            string
	RecoveryCommand string
	Cause           error
}

// TunnelSelectorAmbiguousError requires a stable ID when a selector is both
// a tunnel ID and another tunnel's exact name.
type TunnelSelectorAmbiguousError struct{}

func (*TunnelSelectorAmbiguousError) Error() string {
	return "tunnel selector is ambiguous; retry with the stable tunnel ID shown by `pb tunnel list`"
}

type TunnelRouteSelectorAmbiguousError struct{}

func (*TunnelRouteSelectorAmbiguousError) Error() string {
	return "tunnel route selector is ambiguous; retry with the stable route ID shown by `pb tunnel route list`"
}

type TunnelDomainSelectorAmbiguousError struct{}

func (*TunnelDomainSelectorAmbiguousError) Error() string {
	return "tunnel domain selector is ambiguous; retry with the stable domain ID shown by `pb tunnel domain list`"
}

func (e *TunnelCreateExistingError) Error() string {
	if e == nil {
		return "a tunnel with this name already exists"
	}
	message := fmt.Sprintf("tunnel %q already exists as %s; nothing was changed", e.Name, e.TunnelID)
	if e.RecoveryCommand != "" {
		message += "; inspect it with `" + e.RecoveryCommand + "`"
	}
	if e.Cause != nil {
		message += ": " + e.Cause.Error()
	}
	return message
}

func (e *TunnelCreateExistingError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func (e *TunnelCreateChangedError) Error() string {
	if e == nil {
		return "tunnel creation changed durable state"
	}
	message := fmt.Sprintf("tunnel %s was created, but %s %s; the tunnel was preserved", e.TunnelID, e.Stage, e.Outcome)
	if e.RecoveryCommand != "" {
		message += "; recover with `" + e.RecoveryCommand + "`"
	}
	if e.Cause != nil {
		message += ": " + e.Cause.Error()
	}
	return message
}

func (e *TunnelCreateChangedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

type tunnelConnectorCreateOutput struct {
	Schema               string     `json:"schema"`
	Kind                 string     `json:"kind"`
	TunnelID             string     `json:"tunnel_id"`
	HostID               string     `json:"host_id"`
	ConnectorID          string     `json:"connector_id"`
	OperationID          string     `json:"operation_id"`
	State                string     `json:"state"`
	CredentialGeneration uint64     `json:"credential_generation"`
	ReadyAt              *time.Time `json:"ready_at,omitempty"`
}

type tunnelCreateDomainOutput struct {
	Domain       api.TunnelDomain          `json:"domain"`
	Operation    api.TunnelOperation       `json:"operation"`
	Instructions api.TunnelDNSInstructions `json:"instructions"`
	Replayed     bool                      `json:"replayed"`
	Changed      bool                      `json:"changed"`
}

type tunnelCreateOutput struct {
	Schema    string                       `json:"schema"`
	Kind      string                       `json:"kind"`
	Tunnel    api.Tunnel                   `json:"tunnel"`
	Operation api.TunnelOperation          `json:"operation"`
	Connector *tunnelConnectorCreateOutput `json:"connector,omitempty"`
	Route     *api.TunnelRoute             `json:"route,omitempty"`
	Domains   []tunnelCreateDomainOutput   `json:"domains"`
	Pending   bool                         `json:"pending,omitempty"`
	Recovery  string                       `json:"recovery,omitempty"`
	Replayed  bool                         `json:"replayed"`
	Changed   bool                         `json:"changed"`
}

func tunnelCommand(use, short string, args cobra.PositionalArgs, run func(*cobra.Command, []string) error) *cobra.Command {
	return &cobra.Command{Use: use, Short: short, Args: func(command *cobra.Command, values []string) error {
		if err := commandArgs(args)(command, values); err != nil {
			return err
		}
		for _, value := range values {
			if value == "" || value != strings.TrimSpace(value) || len(value) > 256 || strings.ContainsAny(value, "\x00\r\n/\\") {
				return errors.New("resource argument is invalid")
			}
		}
		return nil
	}, RunE: run, SilenceUsage: true, SilenceErrors: true}
}

func tunnelJSONFlag(command *cobra.Command) {
	command.Flags().Bool("json", false, "print canonical JSON")
}

func tunnelOutput(command *cobra.Command, value any, human string) error {
	jsonOutput, _ := command.Flags().GetBool("json")
	if !jsonOutput {
		_, err := fmt.Fprintln(command.OutOrStdout(), human)
		return err
	}
	encoder := json.NewEncoder(command.OutOrStdout())
	encoder.SetEscapeHTML(true)
	return encoder.Encode(value)
}

func tunnelClient(command *cobra.Command) (*api.Client, context.Context, error) {
	client, err := tunnelClientForCommand(command)
	return client, command.Context(), err
}

func tunnelKey() (string, error) {
	key, err := newTunnelIdempotencyKey()
	if err != nil {
		return "", fmt.Errorf("create idempotency key: %w", err)
	}
	return key, nil
}

func tunnelMutationWaitFlags(command *cobra.Command) {
	command.Flags().Bool("wait", false, "wait for the operation to reach a terminal state")
	command.Flags().Duration("timeout", tunnelOperationWaitDefault, "maximum time to wait for operation completion")
	command.PreRunE = func(command *cobra.Command, _ []string) error {
		wantWait, _ := command.Flags().GetBool("wait")
		if !wantWait {
			return nil
		}
		timeout, _ := command.Flags().GetDuration("timeout")
		if timeout < time.Second || timeout > tunnelOperationWaitMaximum {
			return fmt.Errorf("timeout must be between 1s and %s", tunnelOperationWaitMaximum)
		}
		return nil
	}
}

func waitForTunnelOperation(command *cobra.Command, client *api.Client, initial api.TunnelOperation) (api.TunnelOperation, error) {
	wantWait, _ := command.Flags().GetBool("wait")
	if !wantWait {
		switch initial.State {
		case "pending", "running", "succeeded":
			return initial, nil
		case "failed", "canceled":
			return api.TunnelOperation{}, &TunnelOperationOutcomeError{Operation: initial}
		default:
			return api.TunnelOperation{}, api.ErrUnsafeTunnelResponse
		}
	}
	timeout, _ := command.Flags().GetDuration("timeout")
	if timeout < time.Second || timeout > tunnelOperationWaitMaximum {
		return api.TunnelOperation{}, fmt.Errorf("timeout must be between 1s and %s", tunnelOperationWaitMaximum)
	}
	if client == nil {
		return api.TunnelOperation{}, errors.New("tunnel operation client is unavailable")
	}
	current := initial
	deadlineContext, cancel := context.WithTimeout(command.Context(), timeout)
	defer cancel()
	for {
		switch current.State {
		case "succeeded":
			return current, nil
		case "failed", "canceled":
			return api.TunnelOperation{}, &TunnelOperationOutcomeError{Operation: current}
		case "pending", "running":
		default:
			return api.TunnelOperation{}, api.ErrUnsafeTunnelResponse
		}
		timer := time.NewTimer(tunnelOperationPollInterval)
		select {
		case <-command.Context().Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return api.TunnelOperation{}, command.Context().Err()
		case <-deadlineContext.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return api.TunnelOperation{}, &TunnelOperationWaitTimeoutError{OperationID: initial.ID, Timeout: timeout}
		case <-timer.C:
		}
		next, err := retryTunnelRead(deadlineContext, func() (api.TunnelOperation, error) {
			return client.GetTunnelOperationV1(deadlineContext, initial.ID)
		})
		if err != nil {
			var apiError *api.APIError
			var networkError net.Error
			if errors.As(err, &apiError) && apiError.Retryable() || errors.As(err, &networkError) && networkError.Timeout() {
				continue
			}
			if errors.Is(err, context.DeadlineExceeded) && command.Context().Err() == nil {
				return api.TunnelOperation{}, &TunnelOperationWaitTimeoutError{OperationID: initial.ID, Timeout: timeout}
			}
			return api.TunnelOperation{}, err
		}
		if next.ID != initial.ID || next.ResourceKind != initial.ResourceKind || next.ResourceID != initial.ResourceID {
			return api.TunnelOperation{}, api.ErrUnsafeTunnelResponse
		}
		current = next
	}
}

func tunnelOperationHuman(operation api.TunnelOperation) string {
	if operation.State == "succeeded" {
		return "ready"
	}
	return fmt.Sprintf("%s (%s, operation %s)", operation.Phase, operation.State, operation.ID)
}
