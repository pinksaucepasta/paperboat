package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/preview"
	servepkg "github.com/pinksaucepasta/paperboat/internal/serve"
	"github.com/spf13/cobra"
)

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
	tunnelPageFlag(list)
	inventoryFilterFlags(list)

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

func runPreviewWithDomains(command *cobra.Command, target preview.LeaseTarget, private bool, duration time.Duration, background bool, jsonOutput bool, domains []string) (resultErr error) {
	if !jsonOutput {
		if _, err := fmt.Fprintln(command.ErrOrStderr(), "Preparing preview… Ctrl+C to cancel."); err != nil {
			return previewFailure("preview output failed", err)
		}
	}
	machineID, err := previewMachineID()
	if err != nil {
		return previewFailure(ErrPreviewMachineNotConfigured.Error(), ErrPreviewMachineNotConfigured, err)
	}
	foregroundCtx, cancel := context.WithCancel(command.Context())
	defer cancel()
	// The production observer intentionally receives an empty owner ID. Hostd
	// mints the local owner-session ID and returns it over the loopback lease
	// boundary; injected test carriers retain the generated fallback below.
	carrier, err := newPreviewCarrier(foregroundCtx, target, machineID, "")
	if err != nil {
		return closePreviewCarrier(command.Context(), carrier, err)
	}
	client, err := previewClientForCommand(command)
	if err != nil {
		return closePreviewCarrier(command.Context(), carrier, err)
	}
	if machineCarrier, ok := carrier.(interface {
		MachineAuthSource() (api.MachineAuthSource, error)
	}); ok {
		machineAuth, authErr := machineCarrier.MachineAuthSource()
		if authErr != nil {
			return closePreviewCarrier(command.Context(), carrier, authErr)
		}
		client.SetMachineAuth(machineAuth)
	}
	var leaseClient preview.LeaseClient
	var leaseReader preview.LeaseReader
	domainClient := &previewDomainLeaseClient{
		client: client, domains: append([]string(nil), domains...), instructionsWriter: command.ErrOrStderr(),
	}
	leaseClient, leaseReader = domainClient, domainClient
	if observer, ok := carrier.(interface {
		SetLeaseReader(preview.LeaseReader) error
	}); ok {
		if err := observer.SetLeaseReader(leaseReader); err != nil {
			return closePreviewCarrier(command.Context(), carrier, err)
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
				cleanupErr = previewFailure(ErrPreviewOwnerSessionUnavailable.Error(), ErrPreviewOwnerSessionUnavailable, context.DeadlineExceeded)
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
			heartbeatErr = previewFailure(ErrPreviewOwnerSessionUnavailable.Error(), ErrPreviewOwnerSessionUnavailable, heartbeatErr)
		}
		return previewFailure("preview owner session cleanup failed", cleanupErr, heartbeatErr, releaseErr)
	}
	defer func() {
		if cleanupErr := cleanupOwnerLease(); cleanupErr != nil {
			resultErr = previewFailure("preview failed and owner cleanup was incomplete", resultErr, cleanupErr)
		}
	}()
	ownerSessionKind := preview.OwnerSessionForeground
	needsOwnerLease := false
	if marker, ok := carrier.(interface{ NeedsOwnerSessionLease() bool }); ok {
		needsOwnerLease = marker.NeedsOwnerSessionLease()
	}
	if background && !needsOwnerLease {
		return closePreviewCarrier(command.Context(), carrier, ErrPreviewOwnerSessionUnavailable)
	}
	if needsOwnerLease {
		ownerSessionKind = preview.OwnerSessionLocalLease
		ownerLeaseClient, err = previewOwnerSessionClientForCommand()
		if err != nil {
			return closePreviewCarrier(command.Context(), carrier, previewFailure(ErrPreviewOwnerSessionUnavailable.Error(), ErrPreviewOwnerSessionUnavailable, err))
		}
		if background {
			expiresAt := time.Now().UTC().Add(duration)
			ownerLease, err = ownerLeaseClient.AcquireBackground(foregroundCtx, "", target, expiresAt)
		} else {
			ownerLease, err = ownerLeaseClient.Acquire(foregroundCtx, "", target)
		}
		if err != nil {
			return closePreviewCarrier(command.Context(), carrier, previewFailure(ErrPreviewOwnerSessionUnavailable.Error(), ErrPreviewOwnerSessionUnavailable, err))
		}
		if ownerLease.MachineID != machineID {
			return closePreviewCarrier(command.Context(), carrier, ErrPreviewOwnerSessionUnavailable)
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
		if checker, ok := carrier.(interface {
			CheckOrigin(context.Context, preview.LeaseTarget) error
		}); ok {
			if err := checker.CheckOrigin(foregroundCtx, target); err != nil {
				failure := error(commandRejection{reason: commandRejectPreviewOrigin})
				if foregroundCtx.Err() != nil {
					failure = previewFailure("preview origin check was interrupted", foregroundCtx.Err(), err)
				}
				return closePreviewCarrier(command.Context(), carrier, failure)
			}
		}
	} else {
		ownerSessionID, err = newPreviewOwnerSessionID()
		if err != nil {
			return closePreviewCarrier(command.Context(), carrier, previewFailure(ErrPreviewOwnerSessionUnavailable.Error(), ErrPreviewOwnerSessionUnavailable, err))
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
	if !jsonOutput {
		if _, err := fmt.Fprintln(command.ErrOrStderr(), "Waiting for preview readiness…"); err != nil {
			return closePreviewCarrier(command.Context(), carrier, previewFailure("preview output failed", err))
		}
	}
	readinessTimeout := preview.PreviewPublicationTimeout
	if len(domains) > 0 {
		// Self-hosted preview readiness includes customer DNS and certificate
		// verification. Give the operator time to publish the displayed records.
		readinessTimeout = 15 * time.Minute
	}
	foreground, err := servepkg.StartForeground(foregroundCtx, servepkg.ForegroundConfig{
		Name:           "preview",
		Target:         &targetCopy,
		Duration:       sessionDuration,
		UserDeadline:   userDeadline,
		LeaseClient:    leaseClient,
		Carrier:        carrier,
		OwnerMachineID: machineID,
		OwnerSessionID: ownerSessionID, OwnerSessionKind: ownerSessionKind,
		AccessMode:   accessMode,
		ReadyTimeout: readinessTimeout,
		DrainTimeout: 10 * time.Second,
	})
	if err != nil {
		if errors.Is(err, preview.ErrCustomDomainRequired) {
			return previewFailure("this workspace requires a customer-owned domain for self-hosted previews; rerun with `pb preview --domain <hostname>`, add the displayed DNS records, and retry", err)
		}
		if errors.Is(err, servepkg.ErrPreviewReadinessTimeout) {
			if onlyCause(err, servepkg.ErrPreviewReadinessTimeout) {
				if len(domains) > 0 {
					return commandRejection{reason: commandRejectPreviewDomainTimeout}
				}
				return commandRejection{reason: commandRejectPreviewTimeout}
			}
			return previewFailure("preview readiness timed out", err)
		}
		if onlyCause(err, preview.ErrLeaseExpired) {
			return commandRejection{reason: commandRejectPreviewExpired}
		}
		return previewFailure("preview could not be started", err)
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
				return time.Time{}, previewFailure("preview heartbeat failed", ErrPreviewOwnerSessionUnavailable, err)
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
					return time.Time{}, previewFailure("background preview transfer could not be reconciled", err, reconcileErr)
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
		return previewFailure("preview stopped with an error", consoleErr, outcome.err, waitErr, err)
	}
	var domainObserverCancel context.CancelFunc
	var domainObserverDone <-chan error
	if jsonOutput {
		value := api.PreviewLease{}
		if len(domains) > 0 {
			value, err = client.GetPreviewLease(foregroundCtx, foreground.Lease.ID)
			if err != nil {
				cancel()
				waitErr := foreground.Wait()
				return previewFailure("read preview domains", err, waitErr)
			}
		} else {
			value = apiLeaseFromPreview(foreground.Lease)
		}
		if err := encodePreviewAPILease(command.OutOrStdout(), value); err != nil {
			cancel()
			waitErr := foreground.Wait()
			return previewFailure("preview output failed", err, waitErr)
		}
	} else {
		if err := writePreviewReady(command.OutOrStdout(), foreground.Lease.Endpoint, target); err != nil {
			cancel()
			waitErr := foreground.Wait()
			return previewFailure("preview output failed", err, waitErr)
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
		return previewFailure("preview ended before the requested domain state was reached", waitErr, observerErr)
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
	cursor, limit, err := tunnelPageFlags(command)
	if err != nil {
		return err
	}
	page, err := client.ListPreviewLeasesFiltered(command.Context(), cursor, limit, inventoryFilters(command))
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
	if err := writePreviewLeaseTable(command.OutOrStdout(), page.Items); err != nil {
		return err
	}
	return writeInventoryContinuation(command, nil, page.NextCursor, limit)
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
	values := []string{}
	cursor := ""
	seen := map[string]bool{"": true}
	for {
		page, err := client.ListPreviewLeases(ctx, cursor, 200)
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		for _, item := range page.Items {
			if strings.HasPrefix(strings.ToLower(item.ID), strings.ToLower(toComplete)) {
				values = append(values, item.ID+"\t"+item.State)
			}
		}
		if page.NextCursor == "" {
			break
		}
		if len(page.Items) == 0 || seen[page.NextCursor] {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		seen[page.NextCursor] = true
		cursor = page.NextCursor
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
	id, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}
	return "session_" + id.String(), nil
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
