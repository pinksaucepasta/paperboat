package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/tunnelcreatejournal"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/tunnelenrollment"
	"github.com/pinksaucepasta/paperboat/internal/prompt"
	"github.com/spf13/cobra"
)

func activateCreatedTunnelConnector(command *cobra.Command, tunnelID string) (tunnelConnectorCreateOutput, error) {
	if tunnelConnectorAddRuntime == nil {
		return tunnelConnectorCreateOutput{}, ErrTunnelConnectorAddRequiresRuntime
	}
	var output bytes.Buffer
	runtimeCommand := &cobra.Command{}
	runtimeCommand.SetContext(command.Context())
	runtimeCommand.SetOut(&output)
	runtimeCommand.SetErr(command.ErrOrStderr())
	runtimeCommand.Flags().Bool("json", true, "")
	if err := tunnelConnectorAddRuntime(runtimeCommand, tunnelID); err != nil {
		return tunnelConnectorCreateOutput{}, err
	}
	var projection tunnelenrollment.Projection
	decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&projection) != nil || decoder.Decode(&struct{}{}) != io.EOF || projection.Schema != tunnelenrollment.Schema || projection.Kind != "tunnel_connector" || projection.TunnelID != tunnelID || !validTunnelCLIResourceID(projection.HostID) || !validTunnelCLIResourceID(projection.ConnectorID) || !validTunnelCLIResourceID(projection.OperationID) || projection.State != "ready" || projection.CredentialGeneration == 0 || projection.ReadyAt == nil || projection.ReadyAt.IsZero() {
		return tunnelConnectorCreateOutput{}, api.ErrUnsafeTunnelResponse
	}
	return tunnelConnectorCreateOutput{Schema: projection.Schema, Kind: projection.Kind, TunnelID: projection.TunnelID, HostID: projection.HostID, ConnectorID: projection.ConnectorID, OperationID: projection.OperationID, State: projection.State, CredentialGeneration: projection.CredentialGeneration, ReadyAt: projection.ReadyAt}, nil
}

// resumeCreatedTunnelConnector reconstructs the stable connector projection
// from the control plane after a process crash. The local enrollment endpoint
// is intentionally not called again: an already-recorded ready stage means a
// second enrollment could create a duplicate host attachment.
func resumeCreatedTunnelConnector(ctx context.Context, client *api.Client, journal tunnelcreatejournal.Journal) (tunnelConnectorCreateOutput, error) {
	if ctx == nil || client == nil || !validTunnelCLIResourceID(journal.TunnelID) || !validTunnelCLIResourceID(journal.HostID) {
		return tunnelConnectorCreateOutput{}, api.ErrUnsafeTunnelResponse
	}
	var matches []api.TunnelConnector
	cursor := ""
	seen := map[string]struct{}{cursor: {}}
	for pageNumber := 0; pageNumber < 10; pageNumber++ {
		page, err := retryTunnelRead(ctx, func() (api.TunnelConnectorPage, error) {
			return client.ListTunnelConnectorsV1(ctx, journal.TunnelID, cursor, 200)
		})
		if err != nil {
			return tunnelConnectorCreateOutput{}, err
		}
		for _, connector := range page.Items {
			if connector.TunnelID != journal.TunnelID || connector.HostID != journal.HostID || connector.DesiredState != "active" || connector.ReadyAt == nil || connector.ReadyAt.IsZero() || connector.RotationGeneration <= 0 || connector.CredentialReference == "" {
				continue
			}
			matches = append(matches, connector)
		}
		if page.NextCursor == "" {
			break
		}
		if _, exists := seen[page.NextCursor]; exists {
			return tunnelConnectorCreateOutput{}, api.ErrUnsafeTunnelResponse
		}
		seen[page.NextCursor] = struct{}{}
		cursor = page.NextCursor
	}
	if len(matches) != 1 || !validTunnelCLIResourceID(journal.OperationID) {
		if len(matches) > 1 {
			return tunnelConnectorCreateOutput{}, api.ErrUnsafeTunnelResponse
		}
		return tunnelConnectorCreateOutput{}, tunnelenrollment.ErrUnavailable
	}
	connector := matches[0]
	return tunnelConnectorCreateOutput{
		Schema:               tunnelenrollment.Schema,
		Kind:                 "tunnel_connector",
		TunnelID:             connector.TunnelID,
		HostID:               connector.HostID,
		ConnectorID:          connector.ID,
		OperationID:          journal.OperationID,
		State:                "ready",
		CredentialGeneration: uint64(connector.RotationGeneration),
		ReadyAt:              connector.ReadyAt,
	}, nil
}

func tunnelCreateConnectorError(tunnelID string, err error) error {
	outcome := "failed"
	if errors.Is(err, tunnelenrollment.ErrUnavailable) || errors.Is(err, tunnelenrollment.ErrActivation) || errors.Is(err, context.DeadlineExceeded) {
		outcome = "has an uncertain outcome"
	}
	return &TunnelCreateChangedError{TunnelID: tunnelID, Stage: "connector activation", Outcome: outcome, OutcomeUncertain: outcome != "failed", RecoveryCommand: "pb tunnel connector add " + tunnelID, Cause: err}
}

func tunnelCreateDomainError(tunnelID, hostname, routeID, stage string, err error) error {
	return &TunnelCreateChangedError{TunnelID: tunnelID, Stage: "custom domain " + hostname + " " + stage, Outcome: "failed", RecoveryCommand: fmt.Sprintf("pb tunnel domain add %s %s --route %s", tunnelID, hostname, routeID), Cause: err}
}

func tunnelCreateJournalError(tunnelID, stage, recovery string, err error) error {
	return &TunnelCreateChangedError{
		TunnelID:         tunnelID,
		Stage:            stage,
		Outcome:          "has an uncertain outcome",
		OutcomeUncertain: true,
		RecoveryCommand:  recovery,
		Cause:            err,
	}
}

func tunnelCreateHumanOutput(result tunnelCreateOutput, origin string) string {
	verb := "Created"
	if result.Replayed {
		verb = "Resumed"
	}
	var output strings.Builder
	fmt.Fprintf(&output, "%s tunnel %s (%s)\n", verb, result.Tunnel.Name, result.Tunnel.ID)
	if result.Connector != nil {
		fmt.Fprintf(&output, "Connector %s is ready\n", result.Connector.ConnectorID)
	} else if result.Pending {
		fmt.Fprintln(&output, "Connector activation is pending until a customer domain is verified")
	}
	if result.Tunnel.StableEndpoint != "" {
		fmt.Fprintf(&output, "%s -> %s\n", result.Tunnel.StableEndpoint, origin)
	} else {
		fmt.Fprintln(&output, "Public URL: waiting for a verified customer domain")
		if len(result.Domains) == 0 {
			if result.Route != nil {
				fmt.Fprintf(&output, "Next: add a domain with `pb tunnel domain add %s HOSTNAME --route %s`, then follow `pb tunnel domain instructions %s HOSTNAME`.\n", result.Tunnel.ID, result.Route.ID, result.Tunnel.ID)
			} else {
				fmt.Fprintf(&output, "Next: run `pb tunnel route list %s`, then add a customer domain and follow its DNS instructions.\n", result.Tunnel.ID)
			}
		}
	}
	if result.Route != nil && result.Route.PublicTCPPort != 0 {
		if result.Tunnel.StableEndpoint != "" {
			host, _ := url.Parse(result.Tunnel.StableEndpoint)
			fmt.Fprintf(&output, "TCP endpoint: %s\n", net.JoinHostPort(host.Hostname(), strconv.Itoa(int(result.Route.PublicTCPPort))))
		} else {
			fmt.Fprintln(&output, "TCP endpoint: waiting for a verified customer domain")
		}
	}
	if result.Pending {
		fmt.Fprintf(&output, "Status: pending (%s)", tunnelOperationHuman(result.Operation))
	} else {
		fmt.Fprintf(&output, "Status: %s", tunnelOperationHuman(result.Operation))
	}
	for _, domain := range result.Domains {
		fmt.Fprintf(&output, "\nDNS for %s:", domain.Domain.Hostname)
		for _, record := range domain.Instructions.Records {
			fmt.Fprintf(&output, "\n%s\t%s\t%s\t%d", record.Name, record.Type, record.Value, record.TTL)
		}
		if domain.Instructions.Note != "" {
			fmt.Fprintf(&output, "\n%s", domain.Instructions.Note)
		}
	}
	if result.Recovery != "" {
		fmt.Fprintf(&output, "\nNext: %s", result.Recovery)
	}
	return output.String()
}

var promptTunnelCreateName = prompt.Text

func tunnelCreateNameArgs(command *cobra.Command, args []string) error {
	if len(args) == 1 {
		return nil
	}
	if len(args) == 0 && !jsonOutputRequested(command) && previewInteractiveTerminal(command) {
		return nil
	}
	return invocationError(errors.New("tunnel create requires one name; use `pb tunnel create NAME --port PORT`"))
}

func validateTunnelCreateName(name string) error {
	if !validTunnelCLIName(name, 63) {
		return errors.New("name must be 1-63 ASCII letters, digits, '.', '_' or '-' and cannot start with punctuation")
	}
	return nil
}

func tunnelCreateCommand() *cobra.Command {
	command := tunnelCommand("create <name>", "Create a durable tunnel", tunnelCreateNameArgs, func(command *cobra.Command, args []string) (runErr error) {
		if len(args) == 0 {
			if err := tunnelCreateNameArgs(command, args); err != nil {
				return err
			}
			input, _ := command.InOrStdin().(*os.File)
			name, err := promptTunnelCreateName(prompt.TextOptions{Context: command.Context(), Title: "Create a durable tunnel", Description: "Choose a name for this tunnel. It keeps running after this command exits.", Placeholder: "my-app", Stdin: input, Output: command.ErrOrStderr(), Validate: validateTunnelCreateName})
			if interactiveCanceled(err) {
				return nil
			}
			if err != nil {
				return err
			}
			args = []string{name}
		}
		name := args[0]
		if err := validateTunnelCreateName(name); err != nil {
			return err
		}
		origin, _ := command.Flags().GetString("from")
		port, _ := command.Flags().GetInt("port")
		if (origin == "") == (port == 0) {
			return errors.New("exactly one of --port or --from is required")
		}
		if port != 0 {
			if port < 1 || port > 65535 {
				return errors.New("port must be between 1 and 65535")
			}
			origin = fmt.Sprintf("http://127.0.0.1:%d", port)
		}
		private, _ := command.Flags().GetBool("private")
		expiry, _ := command.Flags().GetDuration("duration")
		if command.Flags().Changed("duration") && expiry <= 0 {
			return errors.New("duration must be greater than zero")
		}
		scheme, address, err := splitTunnelOrigin(origin)
		if err != nil {
			return err
		}
		mode := "public"
		if private {
			mode = "private"
		}
		if team, _ := command.Flags().GetBool("team"); team {
			mode = "team"
		}
		var expires *time.Time
		if expiry > 0 {
			value := time.Now().UTC().Add(expiry)
			expires = &value
		}
		domains, _ := command.Flags().GetStringSlice("domain")
		seenDomains := make(map[string]struct{}, len(domains))
		for i, hostname := range domains {
			hostname = strings.ToLower(strings.TrimSpace(hostname))
			if !validTunnelCLIHostname(hostname) {
				return fmt.Errorf("custom domain %q is invalid", hostname)
			}
			if _, exists := seenDomains[hostname]; exists {
				return fmt.Errorf("custom domain %q was repeated", hostname)
			}
			seenDomains[hostname] = struct{}{}
			domains[i] = hostname
		}
		sort.Strings(domains)
		client, ctx, err := tunnelClient(command)
		if err != nil {
			return err
		}
		if !jsonOutputRequested(command) {
			fmt.Fprintln(command.ErrOrStderr(), "Creating tunnel… Ctrl+C to cancel.")
		}
		requestDigest, err := tunnelCreateRequestDigest(name, mode, scheme, address, expiry, domains)
		if err != nil {
			return err
		}
		workflow, err := beginTunnelCreateWorkflowForCommand(ctx, tunnelCreateWorkflowRequest{Name: name, RequestDigest: requestDigest, DomainCount: len(domains), ExpiresAt: expires})
		if err != nil {
			return fmt.Errorf("begin tunnel create recovery workflow: %w", err)
		}
		defer func() {
			if closeErr := workflow.Close(); closeErr != nil {
				if runErr == nil {
					runErr = closeErr
				} else {
					runErr = errors.Join(runErr, closeErr)
				}
			}
		}()
		journal := workflow.Snapshot()
		hadTunnel := journal.TunnelID != ""
		expires = journal.ExpiresAt
		out, err := retryTunnelRead(ctx, func() (api.TunnelMutation, error) {
			return client.CreateTunnelV1(ctx, api.TunnelCreateInput{Name: name, AccessMode: mode, Origin: api.TunnelOriginInput{Scheme: scheme, Address: address}, ExpiresAt: expires}, journal.TunnelKey)
		})
		if err != nil {
			var apiErr *api.APIError
			if !hadTunnel && errors.As(err, &apiErr) && apiErr.Code == "tunnel_name_conflict" {
				existing, lookupErr := findTunnelByExactName(ctx, client, name)
				if lookupErr == nil {
					if completeErr := workflow.Complete(ctx); completeErr != nil {
						return completeErr
					}
					return &TunnelCreateExistingError{TunnelID: existing.ID, Name: existing.Name, RecoveryCommand: "pb tunnel show " + existing.ID, Cause: err}
				}
			}
			return err
		}
		if hadTunnel {
			if out.Tunnel.ID != journal.TunnelID {
				return api.ErrUnsafeTunnelResponse
			}
			// Completed mutations return the canonical tunnel, while running
			// mutations return their operation. Recover the recorded operation
			// without relying on a replay flag that is not part of the wire API.
			if out.Operation.ID == "" {
				out.Operation, err = retryTunnelRead(ctx, func() (api.TunnelOperation, error) {
					return client.GetTunnelOperationV1(ctx, journal.OperationID)
				})
				if err != nil {
					return tunnelCreateJournalError(out.Tunnel.ID, "recovering tunnel operation", "pb tunnel show "+out.Tunnel.ID, err)
				}
			}
			if out.Operation.ID != journal.OperationID || out.Operation.ResourceKind != "tunnel" || out.Operation.ResourceID != journal.TunnelID {
				return api.ErrUnsafeTunnelResponse
			}
			out.Replayed = true
		}
		if out.Tunnel.Name != name {
			return api.ErrUnsafeTunnelResponse
		}
		if err := workflow.RecordTunnel(ctx, out.Tunnel.ID, out.Operation.ID); err != nil {
			return tunnelCreateJournalError(out.Tunnel.ID, "recording tunnel creation", "pb tunnel show "+out.Tunnel.ID, err)
		}
		result := tunnelCreateOutput{Schema: api.TunnelV1Schema, Kind: "tunnel_create", Tunnel: out.Tunnel, Operation: out.Operation, Domains: make([]tunnelCreateDomainOutput, 0, len(domains)), Replayed: out.Replayed, Changed: out.Changed}
		if len(domains) > 0 || scheme == "tcp" || out.Tunnel.StableEndpoint == "" {
			routes, listErr := retryTunnelRead(ctx, func() (api.TunnelRoutePage, error) {
				return client.ListTunnelRoutesV1(ctx, out.Tunnel.ID, "", 1)
			})
			if listErr != nil || len(routes.Items) != 1 {
				if listErr == nil {
					listErr = errors.New("initial route was not returned")
				}
				return tunnelCreateDomainError(out.Tunnel.ID, strings.Join(domains, ","), "default", "route resolution", listErr)
			}
			result.Route = &routes.Items[0]
			if scheme == "tcp" && (!private && (result.Route.Protocol != "tcp" || result.Route.PublicTCPPort == 0) || private && result.Route.Protocol != "tcp_private") {
				return api.ErrUnsafeTunnelResponse
			}
			journal = workflow.Snapshot()
			for index, hostname := range domains {
				domainState := journal.Domains[index]
				var domain api.TunnelDomainMutation
				var domainErr error
				if domainState.ID != "" {
					domain.Domain, domainErr = retryTunnelRead(ctx, func() (api.TunnelDomain, error) {
						return client.GetTunnelDomainV1(ctx, out.Tunnel.ID, domainState.ID)
					})
					domain.Replayed = true
				} else {
					domain, domainErr = retryTunnelRead(ctx, func() (api.TunnelDomainMutation, error) {
						return client.CreateTunnelDomainV1(ctx, out.Tunnel.ID, domainState.Key, api.TunnelDomainInput{Hostname: hostname, RouteID: routes.Items[0].ID, Provider: "generic"})
					})
				}
				if domainErr != nil {
					return tunnelCreateDomainError(out.Tunnel.ID, hostname, routes.Items[0].ID, "addition", domainErr)
				}
				if domainState.ID != "" && domain.Domain.ID != domainState.ID {
					return api.ErrUnsafeTunnelResponse
				}
				if domain.Domain.TunnelID != out.Tunnel.ID || domain.Domain.RouteID != routes.Items[0].ID || domain.Domain.Hostname != hostname {
					return api.ErrUnsafeTunnelResponse
				}
				if err := workflow.RecordDomain(ctx, index, domain.Domain.ID); err != nil {
					return tunnelCreateJournalError(out.Tunnel.ID, "recording custom domain "+hostname, fmt.Sprintf("pb tunnel domain add %s %s --route %s", out.Tunnel.ID, hostname, routes.Items[0].ID), err)
				}
				instructions, instructionsErr := retryTunnelRead(ctx, func() (api.TunnelDNSInstructions, error) {
					return client.TunnelDomainInstructionsV1(ctx, out.Tunnel.ID, domain.Domain.ID)
				})
				if instructionsErr != nil {
					return &TunnelCreateChangedError{TunnelID: out.Tunnel.ID, Stage: "DNS instructions for custom domain " + hostname, Outcome: "could not be read", RecoveryCommand: fmt.Sprintf("pb tunnel domain instructions %s %s", out.Tunnel.ID, hostname), Cause: instructionsErr}
				}
				result.Domains = append(result.Domains, tunnelCreateDomainOutput{Domain: domain.Domain, Operation: domain.Operation, Instructions: instructions, Replayed: domain.Replayed, Changed: domain.Changed})
			}
		}

		// A self-hosted tunnel has no usable public URL until a customer domain
		// is verified. Persist its domain IDs and show the exact DNS records
		// before attempting connector activation, which cannot become ready
		// while that customer route is ineligible.
		if out.Tunnel.StableEndpoint == "" {
			if len(result.Domains) > 0 && !tunnelCreateDomainsVerified(result.Domains) {
				result.Pending = true
				result.Recovery = tunnelCreatePendingRecovery(result)
				if err := tunnelOutput(command, result, tunnelCreateHumanOutput(result, origin)); err != nil {
					return err
				}
				return nil // Keep the journal so the exact create request can resume.
			}
			refreshed, refreshErr := retryTunnelRead(ctx, func() (api.Tunnel, error) {
				return client.GetTunnelV1(ctx, out.Tunnel.ID)
			})
			if refreshErr != nil {
				return tunnelCreateJournalError(out.Tunnel.ID, "refreshing the verified customer endpoint", "pb tunnel show "+out.Tunnel.ID, refreshErr)
			}
			if refreshed.ID != out.Tunnel.ID || refreshed.Name != out.Tunnel.Name {
				return api.ErrUnsafeTunnelResponse
			}
			out.Tunnel, result.Tunnel = refreshed, refreshed
			if refreshed.StableEndpoint == "" {
				result.Pending = true
				if len(result.Domains) == 0 {
					result.Recovery = tunnelCreatePendingRecovery(result)
				} else {
					result.Recovery = "DNS is verified; rerun the same `pb tunnel create` command after the public customer endpoint is ready."
				}
				if err := tunnelOutput(command, result, tunnelCreateHumanOutput(result, origin)); err != nil {
					return err
				}
				return nil // The endpoint projection may lag DNS verification.
			}
		}
		journal = workflow.Snapshot()
		if err := writeTunnelCreateDNSProgress(command, result.Domains); err != nil {
			hostname := ""
			if len(result.Domains) > 0 {
				hostname = result.Domains[0].Domain.Hostname
			}
			recovery := "pb tunnel show " + out.Tunnel.ID
			if len(result.Domains) > 0 {
				recovery = fmt.Sprintf("pb tunnel domain instructions %s %s", out.Tunnel.ID, hostname)
			}
			return &TunnelCreateChangedError{TunnelID: out.Tunnel.ID, Stage: "showing customer DNS instructions", Outcome: "could not be printed", RecoveryCommand: recovery, Cause: err}
		}
		var connector tunnelConnectorCreateOutput
		if journal.Stage == tunnelcreatejournal.StageConnectorReady || journal.Stage == tunnelcreatejournal.StageDomainsReady {
			connector, err = resumeCreatedTunnelConnector(ctx, client, journal)
			if err != nil {
				return tunnelCreateConnectorError(out.Tunnel.ID, err)
			}
		} else {
			if !jsonOutputRequested(command) {
				fmt.Fprintln(command.ErrOrStderr(), "Waiting for the tunnel connector to become ready…")
			}
			connector, err = activateCreatedTunnelConnector(command, out.Tunnel.ID)
			if err != nil {
				return tunnelCreateConnectorError(out.Tunnel.ID, err)
			}
			if err := workflow.RecordConnectorReady(ctx); err != nil {
				return tunnelCreateJournalError(out.Tunnel.ID, "recording connector readiness", "pb tunnel connector add "+out.Tunnel.ID, err)
			}
		}
		out.Operation, err = waitForTunnelOperation(command, client, out.Operation)
		if err != nil {
			return tunnelCreateJournalError(out.Tunnel.ID, "tunnel readiness", "pb tunnel status "+out.Tunnel.ID, err)
		}
		for index, domain := range result.Domains {
			if domain.Operation.ID == "" {
				domain.Domain, err = waitForCreatedTunnelDomain(command, client, domain.Domain)
			} else {
				domain.Operation, err = waitForTunnelOperation(command, client, domain.Operation)
			}
			if err != nil {
				return tunnelCreateDomainError(out.Tunnel.ID, domain.Domain.Hostname, domain.Domain.RouteID, "readiness wait", err)
			}
			result.Domains[index] = domain
		}
		result.Tunnel, result.Operation, result.Connector = out.Tunnel, out.Operation, &connector
		if err := tunnelOutput(command, result, tunnelCreateHumanOutput(result, origin)); err != nil {
			return err
		}
		if err := workflow.Complete(ctx); err != nil {
			return tunnelCreateJournalError(out.Tunnel.ID, "cleaning up the local create journal", "pb tunnel status "+out.Tunnel.ID, err)
		}
		return nil
	})
	command.Flags().Int("port", 0, "local TCP port")
	command.Flags().String("from", "", "origin URL (http, https, h2c, tcp, or unix)")
	command.Flags().StringSlice("domain", nil, "custom domain to add after creation")
	command.Flags().Bool("private", false, "require authenticated private access")
	command.Flags().Bool("team", false, "require an explicit team grant for browser access")
	command.MarkFlagsMutuallyExclusive("private", "team")
	command.Flags().Duration("duration", 0, "optional tunnel lifetime")
	tunnelMutationWaitFlags(command)
	tunnelJSONFlag(command)
	return command
}

// Recorded domains resume from their canonical state; completed mutation
// responses carry the resource rather than an operation or a replay flag.
func waitForCreatedTunnelDomain(command *cobra.Command, client *api.Client, initial api.TunnelDomain) (api.TunnelDomain, error) {
	wait, _ := command.Flags().GetBool("wait")
	if !wait || initial.State == "ready" {
		return initial, nil
	}
	timeout, _ := command.Flags().GetDuration("timeout")
	if timeout < time.Second || timeout > tunnelOperationWaitMaximum {
		return api.TunnelDomain{}, fmt.Errorf("timeout must be between 1s and %s", tunnelOperationWaitMaximum)
	}
	ctx, cancel := context.WithTimeout(command.Context(), timeout)
	defer cancel()
	current := initial
	for {
		switch current.State {
		case "ready":
			return current, nil
		case "conflict", "dns_error", "tls_error", "expired", "quarantined":
			return api.TunnelDomain{}, fmt.Errorf("customer domain is %s; inspect it with pb tunnel domain show", current.State)
		}
		timer := time.NewTimer(tunnelOperationPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return api.TunnelDomain{}, ctx.Err()
		case <-timer.C:
		}
		next, err := retryTunnelRead(ctx, func() (api.TunnelDomain, error) { return client.GetTunnelDomainV1(ctx, initial.TunnelID, initial.ID) })
		if err != nil {
			return api.TunnelDomain{}, err
		}
		if next.ID != initial.ID || next.RouteID != initial.RouteID || next.Hostname != initial.Hostname {
			return api.TunnelDomain{}, api.ErrUnsafeTunnelResponse
		}
		current = next
	}
}

func tunnelCreateDomainsVerified(domains []tunnelCreateDomainOutput) bool {
	if len(domains) == 0 {
		return false
	}
	for _, domain := range domains {
		if domain.Instructions.VerificationState != "verified" {
			return false
		}
	}
	return true
}

func tunnelCreatePendingRecovery(result tunnelCreateOutput) string {
	if len(result.Domains) == 0 {
		if result.Route != nil {
			return fmt.Sprintf("Add a customer domain with `pb tunnel domain add %s HOSTNAME --route %s`, then rerun this create command.", result.Tunnel.ID, result.Route.ID)
		}
		return "Add a customer domain with `pb tunnel domain add`, follow its DNS instructions, then rerun this create command."
	}
	for _, domain := range result.Domains {
		if domain.Instructions.VerificationState != "verified" {
			return fmt.Sprintf("After adding the displayed DNS records, run `pb tunnel domain verify %s %s`, then rerun this create command.", result.Tunnel.ID, domain.Domain.Hostname)
		}
	}
	return "DNS is verified; rerun this same `pb tunnel create` command when the public customer endpoint is ready."
}

func writeTunnelCreateDNSProgress(command *cobra.Command, domains []tunnelCreateDomainOutput) error {
	wait, _ := command.Flags().GetBool("wait")
	if !wait {
		return nil
	}
	writer := command.ErrOrStderr()
	for _, domain := range domains {
		if domain.Instructions.VerificationState == "verified" {
			continue
		}
		if _, err := fmt.Fprintf(writer, "DNS records for %s are needed before domain readiness; they will also appear in the final output:\n", domain.Domain.Hostname); err != nil {
			return err
		}
		for _, record := range domain.Instructions.Records {
			if _, err := fmt.Fprintf(writer, "%s\t%s\t%s\t%d\n", record.Name, record.Type, record.Value, record.TTL); err != nil {
				return err
			}
		}
		if domain.Instructions.Note != "" {
			if _, err := fmt.Fprintln(writer, domain.Instructions.Note); err != nil {
				return err
			}
		}
	}
	return nil
}

func tunnelCreateRequestDigest(name, mode, scheme, address string, duration time.Duration, domains []string) (string, error) {
	request := struct {
		Schema        string   `json:"schema"`
		Name          string   `json:"name"`
		AccessMode    string   `json:"access_mode"`
		OriginScheme  string   `json:"origin_scheme"`
		OriginAddress string   `json:"origin_address"`
		DurationNanos int64    `json:"duration_nanos"`
		Domains       []string `json:"domains"`
	}{"paperboat.tunnel-create-request/v1", name, mode, scheme, address, int64(duration), append([]string(nil), domains...)}
	body, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]), nil
}
