package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	helperconfig "github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machinecontrol"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/preview"
	servepkg "github.com/pinksaucepasta/paperboat/internal/serve"
	"github.com/spf13/cobra"
)

var (
	// These sentinels let callers and tests branch on a stable failure class
	// without depending on the human-readable detail appended to the error.
	ErrPreviewInvalidTarget           = errors.New("invalid preview target")
	ErrPreviewInvalidDuration         = errors.New("invalid preview duration")
	ErrPreviewCarrierUnavailable      = errors.New("preview carrier unavailable")
	ErrPreviewMachineNotConfigured    = errors.New("preview machine is not configured")
	ErrPreviewOwnerSessionUnavailable = errors.New("preview owner session unavailable")
)

// previewCarrierFactory is the narrow runtime composition seam. TRK-10 owns
// the foreground session lifecycle and TRK-14 owns carrier implementations;
// the CLI must not invent a fake ready state when the runtime has not attached
// a production carrier yet.
type previewCarrierFactory func(context.Context, preview.LeaseTarget, string, string) (preview.Carrier, error)

var newPreviewCarrier previewCarrierFactory = newProductionPreviewCarrier

var previewClientForCommand = func(command *cobra.Command) (*api.Client, error) {
	return backendClient(actionContext(command, nil))
}

var previewMachineID = configuredMachineID

var previewOwnerSessionClientForCommand = newProductionPreviewOwnerSessionClient

const (
	previewBackgroundDefaultTTL = 30 * time.Minute
	previewMaximumTTL           = 24 * time.Hour
)

func newProductionPreviewCarrier(ctx context.Context, target preview.LeaseTarget, machineID, ownerSessionID string) (preview.Carrier, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: context is required", ErrPreviewMachineNotConfigured)
	}
	// The stable host runtime owns the authenticated data carrier. A CLI
	// process only observes the server lease after hostd's DispatchManager has
	// accepted the matching dispatch, so it can never create a competing
	// carrier for the same machine identity.
	_ = target
	_ = ownerSessionID
	stateRoot, err := previewRuntimeStateRoot()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPreviewMachineNotConfigured, err)
	}
	store, err := runtimeIdentityStore()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPreviewMachineNotConfigured, err)
	}
	registration, err := store.Registration()
	if err != nil || registration.ServerURL == "" || registration.MachineID != machineID {
		if err == nil {
			err = fmt.Errorf("registered machine %q does not match selected machine %q", registration.MachineID, machineID)
		}
		return nil, fmt.Errorf("%w: %v", ErrPreviewMachineNotConfigured, err)
	}
	auth, err := machinecontrol.NewSource(machinecontrol.Config{ControlURL: registration.ServerURL, StateRoot: stateRoot})
	if err != nil {
		return nil, fmt.Errorf("%w: machine control source: %v", ErrPreviewMachineNotConfigured, err)
	}
	carrier, err := preview.NewLeaseObserverCarrier(preview.LeaseObserverCarrierConfig{MachineAuth: auth})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPreviewCarrierUnavailable, err)
	}
	return carrier, nil
}

func previewRuntimeStateRoot() (string, error) {
	stateRoot := strings.TrimSpace(os.Getenv("PAPERBOAT_RUNTIME_STATE_ROOT"))
	if stateRoot == "" {
		var err error
		stateRoot, err = helperconfig.DefaultStateRoot(os.Getenv)
		if err != nil {
			return "", err
		}
	}
	if !filepath.IsAbs(stateRoot) {
		return "", errors.New("runtime state root must be absolute")
	}
	return stateRoot, nil
}

func newProductionPreviewOwnerSessionClient() (*preview.LocalOwnerSessionClient, error) {
	stateRoot, err := previewRuntimeStateRoot()
	if err != nil {
		return nil, fmt.Errorf("%w: state root: %v", ErrPreviewOwnerSessionUnavailable, err)
	}
	data, err := readOwnerOnlyFile(filepath.Join(stateRoot, "runtime", "worker-local.json"), 4096)
	if err != nil {
		return nil, fmt.Errorf("%w: local endpoint: %v", ErrPreviewOwnerSessionUnavailable, err)
	}
	var local struct {
		Schema        string `json:"schema"`
		ListenAddress string `json:"listen_address"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&local); err != nil {
		return nil, fmt.Errorf("%w: local endpoint descriptor: %v", ErrPreviewOwnerSessionUnavailable, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF || local.Schema != "paperboat.worker-local/v1" {
		return nil, fmt.Errorf("%w: invalid local endpoint descriptor", ErrPreviewOwnerSessionUnavailable)
	}
	host, portText, err := net.SplitHostPort(strings.TrimSpace(local.ListenAddress))
	if err != nil || (host != "127.0.0.1" && host != "::1") {
		return nil, fmt.Errorf("%w: local endpoint must use 127.0.0.1 or ::1", ErrPreviewOwnerSessionUnavailable)
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return nil, fmt.Errorf("%w: local endpoint port is invalid", ErrPreviewOwnerSessionUnavailable)
	}
	token, err := readOwnerOnlyFile(filepath.Join(stateRoot, "runtime", "local-control-token"), 1024)
	if err != nil || strings.TrimSpace(string(token)) == "" {
		return nil, fmt.Errorf("%w: local control token: %v", ErrPreviewOwnerSessionUnavailable, err)
	}
	client, err := preview.NewLocalOwnerSessionClient("http://"+local.ListenAddress, strings.TrimSpace(string(token)), nil)
	if err != nil {
		return nil, fmt.Errorf("%w: local client: %v", ErrPreviewOwnerSessionUnavailable, err)
	}
	return client, nil
}

// previewCobraCommandV1 is the only public preview command tree.
func previewCobraCommandV1() *cobra.Command {
	return ephemeralTunnelCommand("preview <port|url|path>", "Expose a local target through a temporary preview")
}

// ephemeralTunnelCommand is the sole parser and handler tree for both
// `pb preview` and `pb tunnel --ephemeral`.
func ephemeralTunnelCommand(use, short string) *cobra.Command {
	command := &cobra.Command{
		Use:           use,
		Short:         short,
		Args:          commandArgs(previewTargetArgs),
		RunE:          runPreviewCobra,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	configureEphemeralLaunchFlags(command)

	list := &cobra.Command{
		Use:           "list",
		Short:         "List temporary previews",
		Args:          commandArgs(cobra.NoArgs),
		RunE:          runPreviewListCobra,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	list.Flags().Bool("json", false, "print canonical preview resources as JSON")

	stop := &cobra.Command{
		Use:           "stop <preview>",
		Short:         "Stop a temporary preview",
		Args:          commandArgs(cobra.ExactArgs(1)),
		RunE:          runPreviewStopCobra,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	stop.Flags().Bool("json", false, "print the stopped canonical preview resource as JSON")
	status := &cobra.Command{Use: "status <preview>", Short: "Show temporary preview status", Args: commandArgs(cobra.ExactArgs(1)), RunE: runPreviewStatusCobra, SilenceUsage: true, SilenceErrors: true}
	status.Flags().Bool("json", false, "print the canonical preview resource as JSON")
	deleteCommand := &cobra.Command{Use: "delete <preview>", Short: "Delete a temporary preview", Args: commandArgs(cobra.ExactArgs(1)), RunE: runPreviewStopCobra, SilenceUsage: true, SilenceErrors: true}
	deleteCommand.Flags().Bool("json", false, "print the deleted canonical preview resource as JSON")
	// Inspector views and deliberate replay share the exact tunnel
	// implementation: the alias exposes the same controls, not a second path.
	inspect := previewInspectCommand()
	replay := previewReplayCommand()
	command.AddCommand(list, status, stop, deleteCommand, inspect, replay)
	return command
}

func configureEphemeralLaunchFlags(command *cobra.Command) {
	command.Flags().Bool("private", false, "limit the preview to this account")
	command.Flags().Bool("team", false, "require an explicit team grant for browser access")
	command.MarkFlagsMutuallyExclusive("private", "team")
	command.Flags().Duration("ttl", 0, "bounded preview lifetime (maximum 24h)")
	command.Flags().Duration("duration", 0, "maximum preview lifetime (deprecated: use --ttl)")
	command.Flags().Bool("background", false, "transfer ownership to paperboatd and return after readiness")
	command.Flags().StringArray("domain", nil, "attach a verified custom domain (repeatable)")
	command.Flags().Bool("json", false, "print the canonical preview resource as JSON")
}

func previewTargetArgs(command *cobra.Command, args []string) error {
	jsonOutput, _ := command.Flags().GetBool("json")
	if len(args) == 0 && !jsonOutput && previewInteractiveTerminal(command) {
		return nil
	}
	if len(args) != 1 {
		return fmt.Errorf("accepts 1 arg(s), received %d; usage: pb preview <port|url|path>", len(args))
	}
	_, err := parsePreviewTarget(args[0])
	return err
}

func runPreviewCobra(command *cobra.Command, args []string) error {
	if len(args) == 0 {
		return actionHomePreviewLaunch(command)
	}
	target, err := parsePreviewTarget(args[0])
	if err != nil {
		return err
	}
	ttl, err := command.Flags().GetDuration("ttl")
	if err != nil || ttl < 0 {
		return fmt.Errorf("%w: ttl must be zero or a positive duration", ErrPreviewInvalidDuration)
	}
	legacyDuration, _ := command.Flags().GetDuration("duration")
	if command.Flags().Changed("ttl") && command.Flags().Changed("duration") {
		return fmt.Errorf("%w: use only --ttl", ErrPreviewInvalidDuration)
	}
	if command.Flags().Changed("duration") {
		ttl = legacyDuration
	}
	background, _ := command.Flags().GetBool("background")
	if background && ttl == 0 {
		ttl = previewBackgroundDefaultTTL
	}
	if ttl > previewMaximumTTL {
		return fmt.Errorf("%w: ttl must not exceed 24h", ErrPreviewInvalidDuration)
	}
	private, err := command.Flags().GetBool("private")
	if err != nil {
		return err
	}
	jsonOutput, err := command.Flags().GetBool("json")
	if err != nil {
		return err
	}
	rawDomains, err := command.Flags().GetStringArray("domain")
	if err != nil {
		return err
	}
	domains, err := api.NormalizePreviewDomains(rawDomains)
	if err != nil {
		return err
	}
	return runPreviewWithDomains(command, target, private, ttl, background, jsonOutput, domains)
}

func runPreviewForegroundWithDomains(command *cobra.Command, target preview.LeaseTarget, private bool, duration time.Duration, jsonOutput bool, domains []string) (resultErr error) {
	return runPreviewWithDomains(command, target, private, duration, false, jsonOutput, domains)
}

func runPreviewWithDomains(command *cobra.Command, target preview.LeaseTarget, private bool, duration time.Duration, background bool, jsonOutput bool, domains []string) (resultErr error) {
	machineID, err := previewMachineID()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrPreviewMachineNotConfigured, err)
	}
	foregroundCtx, cancel := context.WithCancel(command.Context())
	defer cancel()
	// The production observer intentionally receives an empty owner ID. Hostd
	// mints the local owner-session ID and returns it over the loopback lease
	// boundary; injected test carriers retain the generated fallback below.
	carrier, err := newPreviewCarrier(foregroundCtx, target, machineID, "")
	if err != nil {
		return err
	}
	client, err := previewClientForCommand(command)
	if err != nil {
		_ = carrier.Close(context.WithoutCancel(command.Context()))
		return err
	}
	if machineCarrier, ok := carrier.(interface {
		MachineAuthSource() (api.MachineAuthSource, error)
	}); ok {
		machineAuth, authErr := machineCarrier.MachineAuthSource()
		if authErr != nil {
			_ = carrier.Close(context.WithoutCancel(command.Context()))
			return authErr
		}
		client.SetMachineAuth(machineAuth)
	}
	var leaseClient preview.LeaseClient
	var leaseReader preview.LeaseReader
	if len(domains) > 0 {
		domainClient := &previewDomainLeaseClient{client: client, domains: append([]string(nil), domains...)}
		leaseClient, leaseReader = domainClient, domainClient
	} else {
		leaseAPIClient, clientErr := preview.NewAPILeaseClient(client)
		if clientErr != nil {
			_ = carrier.Close(context.WithoutCancel(command.Context()))
			return clientErr
		}
		leaseClient, leaseReader = leaseAPIClient, leaseAPIClient
	}
	if observer, ok := carrier.(interface {
		SetLeaseReader(preview.LeaseReader) error
	}); ok {
		if err := observer.SetLeaseReader(leaseReader); err != nil {
			_ = carrier.Close(context.WithoutCancel(command.Context()))
			return err
		}
	}
	var ownerSessionID string
	var ownerLease preview.OwnerSessionLease
	var ownerLeaseClient *preview.LocalOwnerSessionClient
	var heartbeatCancel context.CancelFunc
	var heartbeatDone chan struct{}
	var heartbeatErrors chan error
	var startHeartbeat func()
	retainOwnerLease := false
	cleanupOwnerLease := func() error {
		var cleanupErr error
		if heartbeatCancel != nil {
			heartbeatCancel()
			timer := time.NewTimer(2 * time.Second)
			select {
			case <-heartbeatDone:
				timer.Stop()
			case <-timer.C:
				cleanupErr = fmt.Errorf("%w: heartbeat shutdown timed out", ErrPreviewOwnerSessionUnavailable)
			}
			timer.Stop()
		}
		var heartbeatErr error
		if heartbeatErrors != nil {
			select {
			case heartbeatErr = <-heartbeatErrors:
			default:
			}
		}
		var releaseErr error
		if ownerLeaseClient != nil && ownerLease.ID != "" && !retainOwnerLease {
			releaseCtx, releaseCancel := context.WithTimeout(context.WithoutCancel(command.Context()), 2*time.Second)
			releaseErr = ownerLeaseClient.Release(releaseCtx, ownerLease)
			releaseCancel()
		}
		if heartbeatErr != nil {
			heartbeatErr = fmt.Errorf("%w: %v", ErrPreviewOwnerSessionUnavailable, heartbeatErr)
		}
		return errors.Join(cleanupErr, heartbeatErr, releaseErr)
	}
	defer func() {
		if cleanupErr := cleanupOwnerLease(); cleanupErr != nil {
			resultErr = errors.Join(resultErr, cleanupErr)
		}
	}()
	needsOwnerLease := false
	if marker, ok := carrier.(interface{ NeedsOwnerSessionLease() bool }); ok {
		needsOwnerLease = marker.NeedsOwnerSessionLease()
	}
	if background && !needsOwnerLease {
		_ = carrier.Close(context.WithoutCancel(command.Context()))
		return fmt.Errorf("%w: background mode requires paperboatd ownership", ErrPreviewOwnerSessionUnavailable)
	}
	if needsOwnerLease {
		ownerLeaseClient, err = previewOwnerSessionClientForCommand()
		if err != nil {
			_ = carrier.Close(context.WithoutCancel(command.Context()))
			return err
		}
		if background {
			expiresAt := time.Now().UTC().Add(duration)
			ownerLease, err = ownerLeaseClient.AcquireBackground(foregroundCtx, "", target, expiresAt)
		} else {
			ownerLease, err = ownerLeaseClient.Acquire(foregroundCtx, "", target)
		}
		if err != nil {
			_ = carrier.Close(context.WithoutCancel(command.Context()))
			return fmt.Errorf("%w: acquire: %v", ErrPreviewOwnerSessionUnavailable, err)
		}
		if ownerLease.MachineID != machineID {
			_ = carrier.Close(context.WithoutCancel(command.Context()))
			return fmt.Errorf("%w: hostd leased machine %q, selected machine is %q", ErrPreviewOwnerSessionUnavailable, ownerLease.MachineID, machineID)
		}
		ownerSessionID = ownerLease.OwnerSessionID
		startHeartbeat = func() {
			heartbeatContext, heartbeatStop := context.WithCancel(foregroundCtx)
			heartbeatCancel = heartbeatStop
			heartbeatDone = make(chan struct{})
			heartbeatErrors = make(chan error, 1)
			go func() {
				err := ownerLeaseClient.KeepAlive(heartbeatContext, ownerLease)
				if err != nil {
					heartbeatErrors <- err
					cancel()
				}
				close(heartbeatDone)
			}()
		}
		if !background {
			startHeartbeat()
		}
	} else {
		ownerSessionID, err = newPreviewOwnerSessionID()
		if err != nil {
			return fmt.Errorf("%w: generate owner session: %v", ErrPreviewInvalidTarget, err)
		}
	}
	accessMode := "public"
	if private {
		accessMode = "private"
	}
	if team, _ := command.Flags().GetBool("team"); team {
		accessMode = "team"
	}
	targetCopy := target
	sessionDuration := duration
	var userDeadline *time.Time
	if background {
		deadline := ownerLease.ExpiresAt.UTC()
		userDeadline = &deadline
		sessionDuration = 0
	}
	foreground, err := servepkg.StartForeground(foregroundCtx, servepkg.ForegroundConfig{
		Name:           "preview",
		Target:         &targetCopy,
		Duration:       sessionDuration,
		UserDeadline:   userDeadline,
		LeaseClient:    leaseClient,
		Carrier:        carrier,
		OwnerDeviceID:  machineID,
		OwnerSessionID: ownerSessionID,
		AccessMode:     accessMode,
		ReadyTimeout:   30 * time.Second,
		DrainTimeout:   10 * time.Second,
	})
	if err != nil {
		return err
	}
	if !background && !jsonOutput && previewInteractiveTerminal(command) {
		ended := make(chan error, 1)
		go func() { ended <- foreground.Wait(); close(ended) }()
		transfer := func(ctx context.Context, ttl time.Duration) (time.Time, error) {
			if ownerLeaseClient == nil || startHeartbeat == nil {
				return time.Time{}, ErrPreviewOwnerSessionUnavailable
			}
			deadline := time.Now().UTC().Add(ttl)
			if foreground.Lease.UserDeadline != nil && foreground.Lease.UserDeadline.Before(deadline) {
				deadline = *foreground.Lease.UserDeadline
			}
			heartbeatCancel()
			<-heartbeatDone
			// Drain errors before replacing the heartbeat channels.
			select {
			case err := <-heartbeatErrors:
				return time.Time{}, err
			default:
			}
			updated, err := ownerLeaseClient.TransferBackground(ctx, ownerLease, deadline)
			if err != nil {
				// A heartbeat reconciles a definitive failure. If transfer actually
				// committed, heartbeat conflicts; stop this preview and release its owner
				// rather than claiming or abandoning uncertain background ownership.
				reconcileCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
				_, reconcileErr := ownerLeaseClient.Heartbeat(reconcileCtx, ownerLease)
				stop()
				if reconcileErr != nil {
					cancel()
					return time.Time{}, errors.Join(err, reconcileErr)
				}
				startHeartbeat()
				return time.Time{}, err
			}
			ownerLease = updated
			retainOwnerLease = true
			return updated.ExpiresAt, nil
		}
		outcome, consoleErr := runPreviewConsoleForCommand(command, foreground.Lease, target, accessMode, domains, client, ended, transfer)
		// A canceled UI may not have received a successful handoff response.
		// Only a displayed successful action relinquishes foreground ownership.
		if outcome.action != "background" {
			retainOwnerLease = false
		}
		cancel()
		waitErr := <-ended
		if outcome.output != "" {
			_, err = fmt.Fprint(command.OutOrStdout(), outcome.output)
		}
		return errors.Join(consoleErr, outcome.err, waitErr, err)
	}
	var domainObserverCancel context.CancelFunc
	var domainObserverDone <-chan error
	if jsonOutput {
		value := api.PreviewLease{}
		if len(domains) > 0 {
			value, err = client.GetPreviewLease(foregroundCtx, foreground.Lease.ID)
			if err != nil {
				cancel()
				_ = foreground.Wait()
				return fmt.Errorf("read preview domains: %w", err)
			}
		} else {
			value = apiLeaseFromPreview(foreground.Lease)
		}
		if err := encodePreviewAPILease(command.OutOrStdout(), value); err != nil {
			cancel()
			_ = foreground.Wait()
			return err
		}
	} else {
		if err := writePreviewReady(command.OutOrStdout(), foreground.Lease.Endpoint, target); err != nil {
			cancel()
			_ = foreground.Wait()
			return err
		}
	}
	// Background output is one canonical lease, including its domain projection.
	// Only the foreground process owns a stream of subsequent domain events.
	if len(domains) > 0 && !background {
		observerCtx, observerCancel := context.WithCancel(foregroundCtx)
		domainObserverCancel = observerCancel
		done := make(chan error, 1)
		domainObserverDone = done
		observe := func(domain api.PreviewDomainSummary) error {
			return writePreviewDomainStatus(command.OutOrStdout(), domain)
		}
		if jsonOutput {
			observe = func(domain api.PreviewDomainSummary) error {
				return writePreviewDomainJSON(command.OutOrStdout(), foreground.Lease.ID, domain)
			}
		}
		go func() {
			observeErr := observePreviewDomainChanges(observerCtx, client, foreground.Lease.ID, domains, observe)
			if observeErr != nil {
				cancel()
			}
			done <- observeErr
			close(done)
		}()
	}
	if background {
		cancel()
	}
	waitErr := foreground.Wait()
	var observerErr error
	if domainObserverCancel != nil {
		domainObserverCancel()
		observerErr = <-domainObserverDone
	}
	if err := errors.Join(waitErr, observerErr); err != nil {
		return err
	}
	if background {
		retainOwnerLease = true
	}
	return nil
}

func runPreviewStatusCobra(command *cobra.Command, args []string) error {
	client, err := previewClientForCommand(command)
	if err != nil {
		return err
	}
	lease, err := client.GetPreviewLease(command.Context(), args[0])
	if err != nil {
		return err
	}
	jsonOutput, _ := command.Flags().GetBool("json")
	if jsonOutput {
		return encodePreviewAPILease(command.OutOrStdout(), lease)
	}
	return writePreviewLeaseTable(command.OutOrStdout(), []api.PreviewLease{lease})
}

func runPreviewListCobra(command *cobra.Command, _ []string) error {
	client, err := previewClientForCommand(command)
	if err != nil {
		return err
	}
	page, err := client.ListPreviewLeases(command.Context(), "", 0)
	if err != nil {
		return err
	}
	jsonOutput, err := command.Flags().GetBool("json")
	if err != nil {
		return err
	}
	if jsonOutput {
		return encodePreviewLeasePage(command.OutOrStdout(), page)
	}
	return writePreviewLeaseTable(command.OutOrStdout(), page.Items)
}

func runPreviewStopCobra(command *cobra.Command, args []string) error {
	client, err := previewClientForCommand(command)
	if err != nil {
		return err
	}
	lease, err := client.GetPreviewLease(command.Context(), args[0])
	if err != nil {
		return err
	}
	key, err := api.NewPreviewLeaseIdempotencyKey()
	if err != nil {
		return fmt.Errorf("stop preview: create idempotency key: %w", err)
	}
	stopped, err := client.StopPreviewLease(command.Context(), lease, key)
	if err != nil {
		return err
	}
	jsonOutput, err := command.Flags().GetBool("json")
	if err != nil {
		return err
	}
	if jsonOutput {
		return encodePreviewAPILease(command.OutOrStdout(), stopped)
	}
	_, err = fmt.Fprintf(command.OutOrStdout(), "Stopped preview %s.\n", stopped.ID)
	return err
}

func previewStopCompletion(command *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	ctx, cancel := context.WithTimeout(shellCompletionContext(command), shellCompletionDeadline)
	defer cancel()
	client, err := previewClientForCommand(command)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	page, err := client.ListPreviewLeases(ctx, "", 100)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	values := make([]string, 0, len(page.Items))
	for _, item := range page.Items {
		if strings.HasPrefix(strings.ToLower(item.ID), strings.ToLower(toComplete)) {
			values = append(values, item.ID+"\t"+item.State)
		}
	}
	sort.Strings(values)
	return values, cobra.ShellCompDirectiveNoFileComp
}

func parsePreviewTarget(raw string) (preview.LeaseTarget, error) {
	value := strings.TrimSpace(raw)
	if value == "" || len(value) > 512 || containsPreviewControl(value) {
		return preview.LeaseTarget{}, fmt.Errorf("%w: target is empty or contains invalid characters", ErrPreviewInvalidTarget)
	}
	if isDecimal(value) {
		port, err := strconv.ParseUint(value, 10, 16)
		if err != nil || port == 0 {
			return preview.LeaseTarget{}, fmt.Errorf("%w: port must be between 1 and 65535", ErrPreviewInvalidTarget)
		}
		return preview.LeaseTarget{Scheme: "http", Address: net.JoinHostPort("127.0.0.1", strconv.FormatUint(port, 10))}, nil
	}
	if strings.HasPrefix(value, "/") {
		return parsePreviewUnixPath(value)
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" {
		return preview.LeaseTarget{}, fmt.Errorf("%w: use a port, absolute Unix path, or URL", ErrPreviewInvalidTarget)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme == "unix" {
		if parsed.Host != "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" {
			return preview.LeaseTarget{}, fmt.Errorf("%w: Unix targets must be unix:///absolute/path", ErrPreviewInvalidTarget)
		}
		return parsePreviewUnixPath(parsed.Path)
	}
	if scheme != "http" && scheme != "https" && scheme != "h2c" && scheme != "tcp" {
		return preview.LeaseTarget{}, fmt.Errorf("%w: unsupported target scheme %q", ErrPreviewInvalidTarget, scheme)
	}
	if parsed.User != nil || parsed.Opaque != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Hostname() == "" || parsed.Path != "" && parsed.Path != "/" || parsed.RawPath != "" {
		return preview.LeaseTarget{}, fmt.Errorf("%w: target URL must contain only scheme, host, and port", ErrPreviewInvalidTarget)
	}
	portText := parsed.Port()
	if portText == "" {
		return preview.LeaseTarget{}, fmt.Errorf("%w: target URL must include an explicit port", ErrPreviewInvalidTarget)
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil || port == 0 {
		return preview.LeaseTarget{}, fmt.Errorf("%w: target port must be between 1 and 65535", ErrPreviewInvalidTarget)
	}
	return preview.LeaseTarget{Scheme: scheme, Address: net.JoinHostPort(parsed.Hostname(), strconv.FormatUint(port, 10))}, nil
}

func parsePreviewUnixPath(path string) (preview.LeaseTarget, error) {
	path = strings.TrimSpace(path)
	if !filepath.IsAbs(path) || path == "/" || len(path) > 512 || containsPreviewControl(path) {
		return preview.LeaseTarget{}, fmt.Errorf("%w: Unix target must be a non-root absolute path", ErrPreviewInvalidTarget)
	}
	return preview.LeaseTarget{Scheme: "unix", Address: filepath.Clean(path)}, nil
}

func isDecimal(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func containsPreviewControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func newPreviewOwnerSessionID() (string, error) {
	key, err := api.NewPreviewLeaseIdempotencyKey()
	if err != nil {
		return "", err
	}
	return "session_" + strings.TrimPrefix(key, "preview_"), nil
}

func writePreviewReady(writer io.Writer, endpoint string, target preview.LeaseTarget) error {
	if strings.TrimSpace(endpoint) == "" {
		return fmt.Errorf("%w: readiness did not include an endpoint", preview.ErrSessionInvalid)
	}
	_, err := fmt.Fprintf(writer, "Preview ready\n%s -> %s\n\nPress Ctrl+C to stop\n", endpoint, formatPreviewTarget(target))
	return err
}

func formatPreviewTarget(target preview.LeaseTarget) string {
	if target.Scheme == "unix" {
		return "unix://" + target.Address
	}
	return target.Scheme + "://" + target.Address
}

func encodePreviewAPILease(writer io.Writer, value api.PreviewLease) error {
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func encodePreviewLeasePage(writer io.Writer, page api.PreviewLeasePage) error {
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(page)
}

func writePreviewLeaseTable(writer io.Writer, items []api.PreviewLease) error {
	if len(items) == 0 {
		_, err := io.WriteString(writer, "No active previews.\n")
		return err
	}
	for _, item := range items {
		deadline := item.LeaseDeadline.UTC().Format(time.RFC3339)
		_, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", item.ID, item.AccessMode, formatPreviewTarget(preview.LeaseTarget{Scheme: item.Target.Scheme, Address: item.Target.Address}), item.State, deadline)
		if err != nil {
			return err
		}
	}
	return nil
}

// previewDomainLeaseClient adds the requested aliases to the canonical lease
// create payload while retaining the existing preview session contract. The
// server owns all alias IDs and state; this adapter never synthesizes a ready
// domain or mutates the managed preview endpoint.
type previewDomainLeaseClient struct {
	client  *api.Client
	domains []string
}

func (c *previewDomainLeaseClient) Create(ctx context.Context, request preview.LeaseRequest) (preview.Lease, error) {
	if c == nil || c.client == nil {
		return preview.Lease{}, errors.New("preview domain lease client is unavailable")
	}
	lease, err := c.client.CreatePreviewLease(ctx, api.PreviewLeaseCreateRequest{
		OwnerDeviceID:  request.OwnerDeviceID,
		OwnerSessionID: request.OwnerSessionID,
		Target:         api.PreviewLeaseTarget{Scheme: request.Target.Scheme, Address: request.Target.Address},
		AccessMode:     request.AccessMode,
		ExpiresAt:      request.UserDeadline,
		Domains:        append([]string(nil), c.domains...),
	}, request.IdempotencyKey)
	if err != nil {
		return preview.Lease{}, err
	}
	return previewLeaseFromAPI(lease), nil
}

func (c *previewDomainLeaseClient) Renew(ctx context.Context, lease preview.Lease, idempotencyKey string) (preview.Lease, error) {
	if c == nil || c.client == nil {
		return preview.Lease{}, errors.New("preview domain lease client is unavailable")
	}
	renewed, err := c.client.RenewPreviewLease(ctx, apiLeaseFromPreview(lease), lease.OwnerSessionID, idempotencyKey)
	if err != nil {
		return preview.Lease{}, err
	}
	return previewLeaseFromAPI(renewed), nil
}

func (c *previewDomainLeaseClient) Stop(ctx context.Context, lease preview.Lease, idempotencyKey string) error {
	if c == nil || c.client == nil {
		return errors.New("preview domain lease client is unavailable")
	}
	_, err := c.client.StopPreviewLease(ctx, apiLeaseFromPreview(lease), idempotencyKey)
	return err
}

func (c *previewDomainLeaseClient) Get(ctx context.Context, previewID string) (preview.Lease, error) {
	if c == nil || c.client == nil {
		return preview.Lease{}, errors.New("preview domain lease client is unavailable")
	}
	lease, err := c.client.GetPreviewLease(ctx, previewID)
	if err != nil {
		return preview.Lease{}, err
	}
	return previewLeaseFromAPI(lease), nil
}

func apiLeaseFromPreview(lease preview.Lease) api.PreviewLease {
	return api.PreviewLease{
		Schema:            lease.Schema,
		Kind:              lease.Kind,
		ID:                lease.ID,
		AccountID:         lease.AccountID,
		ActorID:           lease.ActorID,
		OwnerDeviceID:     lease.OwnerDeviceID,
		OwnerSessionID:    lease.OwnerSessionID,
		Target:            api.PreviewLeaseTarget{Scheme: lease.Target.Scheme, Address: lease.Target.Address},
		AccessMode:        lease.AccessMode,
		Persistent:        lease.Persistent,
		Endpoint:          lease.Endpoint,
		LeaseDeadline:     lease.LeaseDeadline,
		UserDeadline:      lease.UserDeadline,
		State:             lease.State,
		AllocationState:   lease.AllocationState,
		EdgeState:         lease.EdgeState,
		OriginState:       lease.OriginState,
		CreatedAt:         lease.CreatedAt,
		LastRenewedAt:     lease.LastRenewedAt,
		CreateOperationID: lease.CreateOperationID,
		ETag:              lease.ETag,
	}
}

func previewLeaseFromAPI(lease api.PreviewLease) preview.Lease {
	return preview.Lease{
		Schema:            lease.Schema,
		Kind:              lease.Kind,
		ID:                lease.ID,
		AccountID:         lease.AccountID,
		ActorID:           lease.ActorID,
		OwnerDeviceID:     lease.OwnerDeviceID,
		OwnerSessionID:    lease.OwnerSessionID,
		Target:            preview.LeaseTarget{Scheme: lease.Target.Scheme, Address: lease.Target.Address},
		AccessMode:        lease.AccessMode,
		Persistent:        lease.Persistent,
		Endpoint:          lease.Endpoint,
		LeaseDeadline:     lease.LeaseDeadline,
		UserDeadline:      lease.UserDeadline,
		State:             lease.State,
		AllocationState:   lease.AllocationState,
		EdgeState:         lease.EdgeState,
		OriginState:       lease.OriginState,
		CreatedAt:         lease.CreatedAt,
		LastRenewedAt:     lease.LastRenewedAt,
		CreateOperationID: lease.CreateOperationID,
		ETag:              lease.ETag,
		Generation:        previewLeaseGeneration(lease.ETag),
	}
}

func previewLeaseGeneration(etag string) int64 {
	value := strings.Trim(etag, `"`)
	parts := strings.Split(value, ":")
	if len(parts) == 4 {
		generation, _ := strconv.ParseInt(parts[3], 10, 64)
		return generation
	}
	return 0
}

func observePreviewDomains(ctx context.Context, client *api.Client, previewID string, requested []string, writer io.Writer) error {
	if writer == nil {
		return errors.New("preview domain writer is required")
	}
	return observePreviewDomainChanges(ctx, client, previewID, requested, func(domain api.PreviewDomainSummary) error { return writePreviewDomainStatus(writer, domain) })
}

func observePreviewDomainChanges(ctx context.Context, client *api.Client, previewID string, requested []string, observe func(api.PreviewDomainSummary) error) error {
	if ctx == nil || client == nil || observe == nil {
		return errors.New("preview domain observer is not configured")
	}
	if len(requested) == 0 {
		return nil
	}
	requested, err := api.NormalizePreviewDomains(requested)
	if err != nil {
		return err
	}
	seen := make(map[string]string, len(requested))
	for {
		lease, err := client.GetPreviewLease(ctx, previewID)
		if err == nil {
			terminal := true
			ready := true
			for _, hostname := range requested {
				domain, ok := previewDomainByHostname(lease.Domains, hostname)
				if !ok {
					terminal = false
					ready = false
					continue
				}
				key := previewDomainObservationKey(domain)
				if seen[hostname] != key {
					seen[hostname] = key
					if err := observe(domain); err != nil {
						return fmt.Errorf("write preview domain status: %w", err)
					}
				}
				if previewDomainReady(domain) {
					continue
				}
				ready = false
				switch domain.State {
				case "released", "expired", "conflict", "dns_error", "tls_error", "quarantined":
				default:
					terminal = false
				}
			}
			if ready || terminal {
				return nil
			}
		}
		timer := time.NewTimer(500 * time.Millisecond)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil
		}
	}
}

func previewDomainByHostname(domains []api.PreviewDomainSummary, hostname string) (api.PreviewDomainSummary, bool) {
	for _, domain := range domains {
		if domain.Hostname == hostname {
			return domain, true
		}
	}
	return api.PreviewDomainSummary{}, false
}

func previewDomainReady(domain api.PreviewDomainSummary) bool {
	return domain.State == "ready" && domain.Certificate.State == "ready"
}

func previewDomainObservationKey(domain api.PreviewDomainSummary) string {
	data, _ := json.Marshal(struct {
		State        string                       `json:"state"`
		DNS          api.PreviewDomainDNS         `json:"dns"`
		Certificate  api.PreviewDomainCertificate `json:"certificate"`
		Instructions *api.PreviewDNSInstructions  `json:"instructions,omitempty"`
	}{domain.State, domain.DNS, domain.Certificate, domain.Instructions})
	return string(data)
}

func writePreviewDomainJSON(writer io.Writer, previewID string, domain api.PreviewDomainSummary) error {
	return writeCLIJSON(writer, struct {
		Event     string                   `json:"event"`
		PreviewID string                   `json:"preview_id"`
		Domain    api.PreviewDomainSummary `json:"domain"`
	}{"domain_status", previewID, domain})
}

func writePreviewDomainStatus(writer io.Writer, domain api.PreviewDomainSummary) error {
	var output strings.Builder
	_, _ = fmt.Fprintf(&output, "Domain %s: state=%s certificate=%s\n", domain.Hostname, domain.State, domain.Certificate.State)
	if domain.Instructions == nil {
		_, err := io.WriteString(writer, output.String())
		return err
	}
	_, _ = fmt.Fprintf(&output, "DNS instructions for %s (%s):\n", domain.Hostname, domain.Instructions.VerificationState)
	for _, record := range domain.Instructions.Records {
		_, _ = fmt.Fprintf(&output, "  %s %s -> %s (TTL %d)\n", record.Type, record.Name, record.Value, record.TTL)
	}
	if domain.Instructions.Note != "" {
		_, _ = fmt.Fprintf(&output, "  %s\n", domain.Instructions.Note)
	}
	_, err := io.WriteString(writer, output.String())
	return err
}
