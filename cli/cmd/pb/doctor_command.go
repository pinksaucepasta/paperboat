package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	sessionauth "github.com/pinksaucepasta/paperboat/internal/auth"
	"github.com/pinksaucepasta/paperboat/internal/config"
	doctorpkg "github.com/pinksaucepasta/paperboat/internal/doctor"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	helperconfig "github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	service "github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/hostruntimecmd"
	"github.com/pinksaucepasta/paperboat/internal/inbox"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/localwait"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
	"github.com/pinksaucepasta/paperboat/shared/httptransport"
	"github.com/pinksaucepasta/paperboat/shared/supportref"
	"github.com/spf13/cobra"
)

func selectStatusMachine(machines []localapi.MachineStatus, target string) (localapi.MachineStatus, error) {
	return localwait.ResolveMachine(machines, target)
}

func writeStatus(output io.Writer, snapshot localapi.Snapshot) {
	fmt.Fprintf(output, "Daemon: %s  Generation: %d  Observed: %s\n", snapshot.DaemonState, snapshot.Generation, snapshot.ObservedAt.Format(time.RFC3339))
	for _, health := range snapshot.Health {
		fmt.Fprintf(output, "Health: %s: %s  Recovery: %s\n", health.Severity, health.Title, health.Recovery)
	}
	for _, machine := range snapshot.Machines {
		path := machine.SelectedPath
		if machine.RelayRegion != "" {
			path += "/" + machine.RelayRegion
		}
		if len(machine.TransportConsumers) > 0 {
			path = transportConsumerSummary(machine.TransportConsumers)
		}
		observed := "never"
		if machine.LastObservedAt != nil {
			observed = machine.LastObservedAt.Format(time.RFC3339)
		}
		fmt.Fprintf(output, "\n%s (%s)\n", machine.Alias, machine.ID)
		fmt.Fprintf(output, "  Runtime: %s  Eligible: %t  Generation: %d  Last observed: %s\n", machine.RuntimeState, machine.Eligible, machine.Generation, observed)
		fmt.Fprintf(output, "  Path: %s  Consumers: %d  Transfer: %s  Preview: %s  SSH: %s\n", path, machine.ActiveConsumers, machine.TransferReadiness, machine.PreviewReadiness, machine.SSHReadiness)
		if len(machine.Tags) > 0 {
			fmt.Fprintf(output, "  Tags: %s\n", strings.Join(machine.Tags, ", "))
		}
		for _, health := range machine.Health {
			fmt.Fprintf(output, "  Health: %s: %s  Recovery: %s\n", health.Severity, health.Title, health.Recovery)
		}
	}
}

func transportConsumerSummary(consumers []localapi.TransportConsumer) string {
	parts := make([]string, 0, len(consumers))
	for _, consumer := range consumers {
		path := consumer.Path
		if consumer.RelayRegion != "" {
			path += "/" + consumer.RelayRegion
		}
		parts = append(parts, fmt.Sprintf("%s=%d", path, consumer.ActiveConsumers))
	}
	return strings.Join(parts, ", ")
}

const (
	doctorRunTimeout              = 20 * time.Second
	doctorProbeTimeout            = 10 * time.Second
	doctorPathReachabilityTimeout = 8 * time.Second
)

func doctorCommandV1() *cobra.Command {
	command := &cobra.Command{
		Use:   "doctor [machine]",
		Short: "Check Paperboat connectivity and readiness",
		Args:  commandArgs(cobra.MaximumNArgs(1)),
		RunE: func(command *cobra.Command, args []string) error {
			if supportref.FromContext(command.Context()) == "" {
				command.SetContext(supportref.WithContext(command.Context(), supportref.New()))
			}
			repair, _ := command.Flags().GetBool("repair")
			if repair {
				if len(args) != 0 {
					return localArgumentError("doctor --repair does not accept a machine")
				}
				if runtime.GOOS == "windows" {
					doctorArgs := []string{"doctor", "--repair"}
					if jsonOutput, _ := command.Flags().GetBool("json"); jsonOutput {
						doctorArgs = append(doctorArgs, "--json")
					}
					if code := hostruntimecmd.Execute(command.Context(), doctorArgs, command.InOrStdin(), command.OutOrStdout(), command.ErrOrStderr()); code != 0 {
						return exitCodeError{code: code}
					}
					return nil
				}
				// Linux and macOS own no privileged dependency: reading the
				// snapshot below already reinstalls and restarts the local
				// daemon service, so repair runs the same checks.
			}
			_, snapshot, err := localDaemonSnapshot(command, installLocalDaemonService)
			if err != nil {
				return fmt.Errorf("read local Paperboat diagnostics: %w", err)
			}
			var machine *localapi.MachineStatus
			var reportMachine *doctorpkg.Machine
			if len(args) == 1 {
				selected, selectErr := selectStatusMachine(snapshot.Machines, args[0])
				if selectErr != nil {
					return selectErr
				}
				machine = &selected
				reportMachine = &doctorpkg.Machine{ID: selected.ID, Alias: selected.Alias}
			}
			report, err := doctorpkg.Run(command.Context(), doctorpkg.Config{
				Timeout: doctorRunTimeout, ProbeTimeout: doctorProbeTimeout, Clock: time.Now,
				Correlation: func() (string, error) { return supportref.FromContext(command.Context()), nil },
			}, reportMachine, doctorProbes(command, snapshot, machine))
			if err != nil {
				return err
			}
			recordDoctorResult(command.Context(), report)
			jsonOutput, _ := command.Flags().GetBool("json")
			if jsonOutput {
				encoder := json.NewEncoder(command.OutOrStdout())
				encoder.SetEscapeHTML(false)
				if err := encoder.Encode(report); err != nil {
					return err
				}
			} else {
				writeDoctorReport(command.OutOrStdout(), report)
			}
			if report.Overall != "healthy" {
				return exitCodeError{code: 1}
			}
			return nil
		},
	}
	command.Flags().Bool("json", false, "print JSON")
	command.Flags().Bool("repair", false, "repair Paperboat-owned local dependencies (Windows) and restart the local daemon, then check")
	return command
}

func doctorProbes(command *cobra.Command, snapshot localapi.Snapshot, machine *localapi.MachineStatus) []doctorpkg.Probe {
	probes := []doctorpkg.Probe{
		{Code: "authentication", Run: func(ctx context.Context) doctorpkg.Check { return probeDoctorAuthentication(ctx, command) }},
		{Code: "daemon", Run: func(context.Context) doctorpkg.Check {
			status := doctorpkg.StatusPass
			recovery := ""
			if snapshot.DaemonState != "ready" {
				status = doctorpkg.StatusWarning
				recovery = "Restart the Paperboat local service and run pb doctor again."
				if snapshot.DaemonState == "starting" {
					// Starting is not a fault (right after pb setup, pb pair or a restart): restarting
					// would only start it again.
					status = doctorpkg.StatusPending
					recovery = "Paperboat is still starting; run `pb doctor` again in a moment."
				}
				if snapshot.DaemonState == "awaiting_enrollment" {
					// Waiting to be added again is not a service fault: restarting cannot help.
					recovery = dashboardEnrollmentGuidance + " Then run `pb doctor` again."
				}
			}
			return doctorpkg.Check{Category: "local", Code: "daemon", Status: status, Summary: "The local daemon is " + snapshot.DaemonState + ".", Recovery: recovery}
		}},
		{Code: "local_state", Run: func(ctx context.Context) doctorpkg.Check { return probeDoctorLocalState(ctx) }},
		{Code: "clock", Run: func(ctx context.Context) doctorpkg.Check { return probeDoctorClock(ctx, command) }},
		{Code: "machine_guard_version", Run: func(ctx context.Context) doctorpkg.Check { return probeDoctorGuardVersion(ctx) }},
		{Code: "machine_credentials", Run: func(ctx context.Context) doctorpkg.Check { return probeDoctorCredentials(ctx) }},
		{Code: "udp_ipv4", Run: func(ctx context.Context) doctorpkg.Check {
			return probeDoctorUDP(ctx, "udp4", "0.0.0.0:0", "udp_ipv4", "IPv4")
		}},
		{Code: "udp_ipv6", Run: func(ctx context.Context) doctorpkg.Check {
			return probeDoctorUDP(ctx, "udp6", "[::]:0", "udp_ipv6", "IPv6")
		}},
	}
	probes = append(probes, doctorLingerProbes()...)
	probes = append(probes, doctorCarrierProbes()...)
	if machine != nil {
		probes = append(probes, doctorMachineProbes(*machine)...)
		probes = append(probes, doctorPathReachabilityProbes(command, machine.ID)...)
	}
	return probes
}

func doctorPathReachabilityProbes(command *cobra.Command, machineID string) []doctorpkg.Probe {
	return doctorNativeReachabilityProbe(command.Context(), doctorPathReachabilityTimeout, func(ctx context.Context) (tunnel.NativeProbe, error) {
		commandContext := actionContext(command, []string{machineID})
		commandContext.Context = ctx
		dependencies, err := buildDeps(commandContext)
		if err != nil {
			return tunnel.NativeProbe{}, err
		}
		if dependencies.peerLocal == nil {
			return tunnel.NativeProbe{}, errors.New("peer tunnel is unavailable")
		}
		client, err := backendClient(commandContext)
		if err != nil {
			return tunnel.NativeProbe{}, err
		}
		machine, err := resolveUserMachine(ctx, client, machineID)
		if err != nil {
			return tunnel.NativeProbe{}, err
		}
		if !machine.Online {
			return tunnel.NativeProbe{}, errors.New("selected machine is offline")
		}
		target := resolver.ConnectInfo{TargetKind: "machine", MachineID: machine.ID, Machine: machine.Alias, MachineGeneration: uint64(machine.InstallationGeneration), Terminal: &resolver.TerminalTarget{Protocol: "paperboat.health-probe.v1", EnvironmentID: machine.EnvironmentID}}
		return probeDaemonPeer(ctx, dependencies.peerLocal, target, "doctor_native")
	})
}

func doctorNativeReachabilityProbe(parent context.Context, timeout time.Duration, load func(context.Context) (tunnel.NativeProbe, error)) []doctorpkg.Probe {
	return []doctorpkg.Probe{{Code: "peer_reachability", Run: func(probeCtx context.Context) doctorpkg.Check {
		if parent == nil || probeCtx == nil || timeout <= 0 || load == nil {
			return doctorpkg.Check{Category: "transport", Code: "peer_reachability", Status: doctorpkg.StatusUnavailable, Summary: "Native peer reachability could not be checked.", Recovery: "Check the local Paperboat service and run pb doctor again."}
		}
		ctx, cancel := context.WithTimeout(probeCtx, timeout)
		defer cancel()
		stopParentCancellation := context.AfterFunc(parent, cancel)
		defer stopParentCancellation()
		if parent.Err() != nil {
			cancel()
		}
		result, err := load(ctx)
		if ctx.Err() != nil {
			cause := err
			if cause == nil {
				cause = ctx.Err()
			} else if cause != ctx.Err() {
				cause = errors.Join(cause, ctx.Err())
			}
			if !errorreport.HTTPAttemptObserved(cause) {
				errorreport.Current().ObserveFailure(ctx, "pb", "doctor", "peer_connect", "command_failed", cause)
			}
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return doctorpkg.Check{Category: "transport", Code: "peer_reachability", Status: doctorpkg.StatusUnavailable, Summary: "Native peer reachability check timed out.", Recovery: "Check the selected machine and network, then run pb doctor again."}
			}
			return doctorpkg.Check{Category: "transport", Code: "peer_reachability", Status: doctorpkg.StatusUnavailable, Summary: "Native peer reachability check was canceled.", Recovery: "Run pb doctor again when the selected machine and network are available."}
		}
		if err != nil {
			errorreport.Current().ObserveFailure(ctx, "pb", "doctor", "peer_connect", "command_failed", err)
			return doctorpkg.Check{Category: "transport", Code: "peer_reachability", Status: doctorpkg.StatusFail, Summary: "Could not establish an authenticated native connection to the selected machine.", Recovery: "Check the Paperboat service on the selected machine and local network access, then run pb doctor again."}
		}
		return doctorPeerCheck(result)
	}}}
}

func doctorPeerCheck(result tunnel.NativeProbe) doctorpkg.Check {
	check := doctorpkg.Check{Category: "transport", Code: "peer_reachability", Status: doctorpkg.StatusPass, Summary: "Established an authenticated native connection to the selected machine.", SelectedPath: result.Path}
	switch result.Path {
	case "direct":
	case "peer_relay", "regional_relay":
		check.Status = doctorpkg.StatusWarning
		check.Summary = "Established an authenticated native connection through a relay."
		check.Recovery = "Paperboat will retry direct connectivity automatically; check UDP availability if relay use persists."
	case "unknown":
		check.Summary = "Established an authenticated native connection; its underlay path is not currently observed."
	default:
		check.Status = doctorpkg.StatusUnavailable
		check.Summary = "Native connection path could not be classified."
	}
	return check
}

// doctorCredentialFailure says what is wrong when the saved sign-in cannot be used. A sign-in the
// server rejected for good (the usual result of removing the computer from the account in the
// console) is named as such; a missing sign-in points at pb login too.
func doctorCredentialFailure(check doctorpkg.Check, err error) doctorpkg.Check {
	var signedOut *sessionauth.SignedOutError
	// The server rejecting the stored credential at refresh is the same fact as a signed-out sign-in.
	if errors.As(err, &signedOut) || errors.Is(err, api.ErrUnauthenticated) {
		check.Summary = "This computer is signed out of Paperboat: its sign-in was revoked, for example because the computer was removed from the account."
		check.Recovery = dashboardEnrollmentGuidance + " That signs in again and adds this computer back. Then run `pb doctor` again."
		return check
	}
	if onlyEnvironmentFailureLeaves(err, func(leaf error) bool {
		return leaf == config.ErrNoCredentials || leaf == config.ErrSecretNotFound || leaf == api.ErrUnauthenticated
	}) {
		check.Recovery = dashboardEnrollmentGuidance + " Then run `pb doctor` again."
	}
	return check
}

func probeDoctorAuthentication(ctx context.Context, command *cobra.Command) doctorpkg.Check {
	check := doctorpkg.Check{Category: "control", Code: "authentication", Status: doctorpkg.StatusFail, Summary: "Paperboat authentication is unavailable.", Recovery: "Check local credential storage and the Paperboat service, then run `pb doctor` again."}
	cfg, err := config.Load(configPathFlag(command))
	if err != nil {
		errorreport.Current().ObserveFailure(ctx, "pb", "doctor", "command", "command_failed", err)
		check.Summary = "The Paperboat configuration could not be loaded."
		check.Recovery = "Repair the Paperboat configuration and run pb doctor again."
		return check
	}
	if server, _ := command.Flags().GetString("server"); strings.TrimSpace(server) != "" {
		cfg.ServerURL, err = config.NormalizeServerURL(server)
		if err != nil {
			check.Summary = "The configured Paperboat server URL is invalid."
			check.Recovery = "Correct the Paperboat server URL and run pb doctor again."
			return check
		}
	}
	if strings.TrimSpace(cfg.ServerURL) == "" {
		check.Summary = "The Paperboat server is not configured."
		check.Recovery = "Configure the Paperboat server and run pb doctor again."
		return check
	}
	auth, err := sessionauth.NewSource(cfg)
	if err != nil {
		errorreport.Current().ObserveFailure(ctx, "pb", "doctor", "command", "command_failed", err)
		return check
	}
	credential, err := auth.Credential()
	if err != nil {
		errorreport.Current().ObserveFailure(ctx, "pb", "doctor", "command", "command_failed", err)
		return doctorCredentialFailure(check, err)
	}
	if _, err := api.New(cfg.ServerURL, credential, nil).Me(ctx); err != nil {
		if diagnosis, ok := doctorProxyDiagnosis(err); ok {
			check.Summary = "The Paperboat server is unreachable through the configured proxy."
			check.Recovery = diagnosis.Recovery
		} else if outdated := doctorUnsupportedVersion(err); outdated != nil {
			check.Summary = "This Paperboat version is no longer supported by the server."
			check.Recovery = "Run `pb update` to install a supported release, then run pb doctor again."
		} else {
			check.Summary = "The Paperboat server did not accept the authenticated health check."
			check.Recovery = "Check network access and sign in again if needed, then run pb doctor again."
		}
		return check
	}
	return doctorpkg.Check{Category: "control", Code: "authentication", Status: doctorpkg.StatusPass, Summary: "The Paperboat server accepted the current credential."}
}

func probeDoctorLocalState(ctx context.Context) doctorpkg.Check {
	state := collectLocalDoctor(ctx)
	if state.SetupState == "configured" && state.IdentityState == "valid" {
		return doctorpkg.Check{Category: "local", Code: "local_state", Status: doctorpkg.StatusPass, Summary: "Local Paperboat state and identity are valid."}
	}
	recovery := "Run pb setup, then run pb doctor again."
	if len(state.RecoveryActions) > 0 {
		recovery = sentence(state.RecoveryActions[0])
	}
	return doctorpkg.Check{Category: "local", Code: "local_state", Status: doctorpkg.StatusWarning, Summary: "Local Paperboat runtime state is not fully configured.", Recovery: recovery}
}

func probeDoctorUDP(ctx context.Context, network, address, code, family string) doctorpkg.Check {
	packet, err := (&net.ListenConfig{}).ListenPacket(ctx, network, address)
	if err != nil {
		errorreport.Current().ObserveFailure(ctx, "pb", "doctor", "listener_bind", "command_failed", err)
		return doctorpkg.Check{Category: "network", Code: code, Status: doctorpkg.StatusWarning, Summary: family + " UDP sockets are unavailable.", Recovery: "Check local firewall and network settings, then run pb doctor again."}
	}
	return finishDoctorUDP(ctx, packet, code, family)
}

func finishDoctorUDP(ctx context.Context, packet io.Closer, code, family string) doctorpkg.Check {
	if err := packet.Close(); err != nil {
		errorreport.Current().ObserveFailure(ctx, "pb", "doctor", "listener_bind", "command_failed", err)
		return doctorpkg.Check{Category: "network", Code: code, Status: doctorpkg.StatusWarning, Summary: family + " UDP socket cleanup failed.", Recovery: "Check the local Paperboat service, then run pb doctor again."}
	}
	return doctorpkg.Check{Category: "network", Code: code, Status: doctorpkg.StatusPass, Summary: family + " UDP sockets are available."}
}

func doctorMachineProbes(machine localapi.MachineStatus) []doctorpkg.Probe {
	return []doctorpkg.Probe{
		{Code: "machine_runtime", Run: func(context.Context) doctorpkg.Check {
			if machine.Eligible && machine.RuntimeState == "ready" {
				return doctorpkg.Check{Category: "machine", Code: "machine_runtime", Status: doctorpkg.StatusPass, Summary: "The selected machine runtime is ready."}
			}
			return doctorpkg.Check{Category: "machine", Code: "machine_runtime", Status: doctorpkg.StatusFail, Summary: "The selected machine runtime is not ready.", Recovery: "Check the Paperboat service on the selected machine and run pb doctor again."}
		}},
		{Code: "selected_path", Run: func(context.Context) doctorpkg.Check {
			switch machine.SelectedPath {
			case "direct":
				return doctorpkg.Check{Category: "transport", Code: "selected_path", Status: doctorpkg.StatusPass, Summary: "The active Paperboat path is direct."}
			case "relay":
				return doctorpkg.Check{Category: "transport", Code: "selected_path", Status: doctorpkg.StatusWarning, Summary: "The active Paperboat path uses a regional relay.", Recovery: "Direct connectivity is retried automatically; check UDP availability if relay use persists."}
			case "wss":
				return doctorpkg.Check{Category: "transport", Code: "selected_path", Status: doctorpkg.StatusWarning, Summary: "The active Paperboat path uses WebSocket fallback.", Recovery: "Check UDP and QUIC access; Paperboat will retry stronger paths automatically."}
			default:
				return doctorpkg.Check{Category: "transport", Code: "selected_path", Status: doctorpkg.StatusUnavailable, Summary: "No active Paperboat path is currently observed."}
			}
		}},
		{Code: "nat_mapping_ipv4", Run: func(context.Context) doctorpkg.Check { return doctorNATCheck("ipv4", machine.NATMappingIPv4) }},
		{Code: "nat_mapping_ipv6", Run: func(context.Context) doctorpkg.Check { return doctorNATCheck("ipv6", machine.NATMappingIPv6) }},
		{Code: "captive_portal", Run: func(context.Context) doctorpkg.Check { return doctorCaptivePortalCheck(machine.CaptivePortal) }},
		{Code: "path_mtu", Run: func(context.Context) doctorpkg.Check { return doctorPMTUCheck(machine.PMTU) }},
		{Code: "router_protocol", Run: func(context.Context) doctorpkg.Check { return doctorRouterProtocolCheck(machine.RouterProtocol) }},
		{Code: "router_mapping", Run: func(context.Context) doctorpkg.Check { return doctorRouterMappingCheck(machine.RouterMapping) }},
		{Code: "mapping_lifetime", Run: func(context.Context) doctorpkg.Check { return doctorMappingLifetimeCheck(machine.MappingLifetime) }},
		{Code: "update_health", Run: func(context.Context) doctorpkg.Check { return doctorUpdateHealthCheck(machine.UpdateHealth) }},
		{Code: "ssh_readiness", Run: func(context.Context) doctorpkg.Check {
			switch machine.SSHReadiness {
			case "ready":
				return doctorpkg.Check{Category: "ssh", Code: "ssh_readiness", Status: doctorpkg.StatusPass, Summary: "Managed SSH is ready."}
			case "degraded":
				return doctorpkg.Check{Category: "ssh", Code: "ssh_readiness", Status: doctorpkg.StatusWarning, Summary: "Managed SSH requires attention.", Recovery: "Run pb ssh doctor for the selected machine."}
			default:
				return doctorpkg.Check{Category: "ssh", Code: "ssh_readiness", Status: doctorpkg.StatusUnavailable, Summary: "Managed SSH readiness is unavailable."}
			}
		}},
	}
}

func doctorRouterProtocolCheck(value string) doctorpkg.Check {
	check := doctorpkg.Check{Category: "network", Code: "router_protocol"}
	switch value {
	case "pcp":
		check.Status = doctorpkg.StatusPass
		check.Summary = "The verified router mapping uses PCP."
	case "nat_pmp":
		check.Status = doctorpkg.StatusPass
		check.Summary = "The verified router mapping uses NAT-PMP."
	case "upnp":
		check.Status = doctorpkg.StatusPass
		check.Summary = "The verified router mapping uses UPnP."
	case "none":
		check.Status = doctorpkg.StatusUnavailable
		check.Summary = "This network did not provide a router mapping protocol."
	default:
		check.Status = doctorpkg.StatusUnavailable
		check.Summary = "The router mapping protocol could not be observed."
	}
	return check
}

func doctorMappingLifetimeCheck(value string) doctorpkg.Check {
	check := doctorpkg.Check{Category: "network", Code: "mapping_lifetime"}
	switch value {
	case "30s_to_2m", "2m_to_10m", "over_10m":
		check.Status = doctorpkg.StatusPass
		check.Summary = "The authenticated UDP mapping lifetime supports adaptive keepalive."
	case "under_30s":
		check.Status = doctorpkg.StatusWarning
		check.Summary = "The authenticated UDP mapping lifetime is short."
		check.Recovery = "Keep relay fallback available; check router UDP timeout settings if direct connections are unstable."
	default:
		check.Status = doctorpkg.StatusUnavailable
		check.Summary = "An authenticated UDP mapping lifetime measurement is not available."
	}
	return check
}

func doctorRouterMappingCheck(value string) doctorpkg.Check {
	check := doctorpkg.Check{Category: "network", Code: "router_mapping"}
	switch value {
	case "verified":
		check.Status = doctorpkg.StatusPass
		check.Summary = "A router mapping was verified from the owned direct-path socket."
	case "unreachable":
		check.Status = doctorpkg.StatusWarning
		check.Summary = "A router mapping could not be verified from outside the local network."
		check.Recovery = "Check router and firewall UDP policy; relay fallback remains available."
	case "untrusted":
		check.Status = doctorpkg.StatusUnavailable
		check.Summary = "Router mapping is disabled on this network by Paperboat policy."
	case "unavailable":
		check.Status = doctorpkg.StatusUnavailable
		check.Summary = "This network does not provide a usable router mapping."
	default:
		check.Status = doctorpkg.StatusUnavailable
		check.Summary = "Router mapping status could not be observed."
	}
	return check
}

func doctorUpdateHealthCheck(value string) doctorpkg.Check {
	check := doctorpkg.Check{Category: "update", Code: "update_health"}
	switch value {
	case "healthy":
		check.Status = doctorpkg.StatusPass
		check.Summary = "Signed update health is ready."
	case "recovery_required":
		check.Status = doctorpkg.StatusFail
		check.Summary = "Signed update recovery requires attention."
		check.Recovery = "Restart the Paperboat services; if the failure remains, reinstall the current signed release."
	default:
		check.Status = doctorpkg.StatusUnavailable
		check.Summary = "Signed update health is unavailable."
	}
	return check
}

func doctorNATCheck(family, mapping string) doctorpkg.Check {
	base := doctorpkg.Check{Category: "network", Code: "nat_mapping_" + family}
	switch mapping {
	case "endpoint_independent":
		base.Status = doctorpkg.StatusPass
		base.Summary = "The observed " + family + " NAT mapping is endpoint-independent."
	case "destination_dependent":
		base.Status = doctorpkg.StatusWarning
		base.Summary = "The observed " + family + " NAT mapping depends on the destination."
		base.Recovery = "Keep relay fallback available and run pb doctor again from another network."
	default:
		base.Status = doctorpkg.StatusUnavailable
		base.Summary = "The " + family + " NAT mapping could not be observed."
	}
	return base
}

func doctorCaptivePortalCheck(value string) doctorpkg.Check {
	check := doctorpkg.Check{Category: "network", Code: "captive_portal"}
	switch value {
	case "clear":
		check.Status = doctorpkg.StatusPass
		check.Summary = "No captive portal was observed."
	case "suspected":
		check.Status = doctorpkg.StatusWarning
		check.Summary = "This network may require captive portal sign-in."
		check.Recovery = "Complete the network sign-in, then run pb doctor again."
	default:
		check.Status = doctorpkg.StatusUnavailable
		check.Summary = "Captive portal status could not be observed."
	}
	return check
}

func doctorPMTUCheck(value string) doctorpkg.Check {
	check := doctorpkg.Check{Category: "network", Code: "path_mtu"}
	switch value {
	case "standard", "extended":
		check.Status = doctorpkg.StatusPass
		check.Summary = "The authenticated path MTU is suitable for direct QUIC."
	case "minimum_1200":
		check.Status = doctorpkg.StatusWarning
		check.Summary = "The authenticated path supports only the minimum QUIC packet size."
		check.Recovery = "Check VPN and router MTU settings; relay fallback remains available."
	case "below_quic_floor":
		check.Status = doctorpkg.StatusFail
		check.Summary = "The authenticated path MTU is below the QUIC minimum."
		check.Recovery = "Check VPN and router MTU settings, then run pb doctor again."
	default:
		check.Status = doctorpkg.StatusUnavailable
		check.Summary = "An authenticated path MTU measurement is not available."
	}
	return check
}

func writeDoctorReport(output io.Writer, report doctorpkg.Report) {
	fmt.Fprintf(output, "Paperboat doctor: %s\n", report.Overall)
	fmt.Fprintf(output, "Correlation: %s\n", report.CorrelationID)
	if report.Machine != nil {
		fmt.Fprintf(output, "Machine: %s (%s)\n", report.Machine.Alias, report.Machine.ID)
	}
	for _, check := range report.Checks {
		fmt.Fprintf(output, "%s: %s: %s\n", check.Code, check.Status, check.Summary)
		if check.Recovery != "" {
			fmt.Fprintf(output, "  Recovery: %s\n", check.Recovery)
		}
	}
}

type proxyDoctorDiagnosis struct {
	State    string `json:"state"`
	Recovery string `json:"recovery"`
}

func doctorProxyDiagnosis(err error) (proxyDoctorDiagnosis, bool) {
	var proxyErr *httptransport.ProxyError
	if !errors.As(err, &proxyErr) {
		return proxyDoctorDiagnosis{}, false
	}
	switch proxyErr.Failure {
	case httptransport.ProxyAutomaticConfigurationUnsupported:
		return proxyDoctorDiagnosis{
			State:    "pac_unsupported",
			Recovery: "This network requires PAC/WPAD, which Paperboat does not execute. Configure a credential-free explicit HTTPS proxy with HTTPS_PROXY (or PAPERBOAT_HTTPS_PROXY for a managed service), then retry `pb doctor`.",
		}, true
	case httptransport.ProxyAuthenticationRequired:
		return proxyDoctorDiagnosis{
			State:    "authentication_required",
			Recovery: "The configured proxy requires authentication, which Paperboat does not accept in proxy URLs. Configure a credential-free explicit proxy, then retry `pb doctor`.",
		}, true
	case httptransport.ProxyInvalid:
		return proxyDoctorDiagnosis{
			State:    "invalid_configuration",
			Recovery: "Configure a credential-free http:// or https:// proxy URL with no path, query, or fragment, then retry `pb doctor`.",
		}, true
	default:
		return proxyDoctorDiagnosis{}, false
	}
}

type localDoctorReport struct {
	failure                error
	StateRoot              string   `json:"state_root,omitempty"`
	SetupState             string   `json:"setup_state"`
	MachineID              string   `json:"machine_id,omitempty"`
	EnvironmentID          string   `json:"environment_id,omitempty"`
	InstallationGeneration int64    `json:"installation_generation,omitempty"`
	IdentityState          string   `json:"identity_state"`
	CredentialState        string   `json:"machine_control_credential"`
	InboxPath              string   `json:"inbox_path,omitempty"`
	InboxState             string   `json:"inbox_state"`
	ConfigService          string   `json:"config_service"`
	HostRuntime            string   `json:"host_runtime"`
	TrackedSessions        uint64   `json:"tracked_sessions"`
	ActiveProcesses        uint64   `json:"active_processes"`
	ActiveAttachments      uint64   `json:"active_attachments"`
	ActiveUploads          uint64   `json:"active_uploads"`
	WorkloadCounts         string   `json:"workload_counts_state"`
	RecoveryActions        []string `json:"recovery_actions,omitempty"`
}

func collectLocalDoctor(ctx context.Context) (report localDoctorReport) {
	defer func() { observeLocalDoctorFailure(ctx, report.failure) }()
	report = localDoctorReport{SetupState: "not_set_up", IdentityState: "missing", CredentialState: "missing", InboxState: "unconfigured", ConfigService: "not_installed", HostRuntime: "not_paired", WorkloadCounts: "unavailable"}
	stateRoot := strings.TrimSpace(os.Getenv("PAPERBOAT_RUNTIME_STATE_ROOT"))
	if stateRoot == "" {
		root, err := helperconfig.DefaultStateRoot(os.Getenv)
		if err != nil {
			report.failure = err
			report.SetupState = "error"
			report.RecoveryActions = append(report.RecoveryActions, "set PAPERBOAT_RUNTIME_STATE_ROOT to an absolute private directory")
			return report
		}
		stateRoot = root
	}
	report.StateRoot = stateRoot
	identityPath := filepath.Join(stateRoot, "machine-identity.json")
	registrationPath := filepath.Join(stateRoot, "machine-registration.json")
	if _, err := os.Lstat(registrationPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			report.RecoveryActions = append(report.RecoveryActions, "run pb setup")
		} else {
			report.failure = err
			report.SetupState, report.IdentityState = "unavailable", "unavailable"
			report.RecoveryActions = append(report.RecoveryActions, "check access to the Paperboat state directory before changing this installation")
		}
		return report
	}
	if info, err := os.Lstat(identityPath); err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		report.failure = err
		report.SetupState, report.IdentityState = "invalid", "invalid"
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			report.SetupState, report.IdentityState = "unavailable", "unavailable"
			report.RecoveryActions = append(report.RecoveryActions, "check access to the Paperboat state directory before changing this installation")
			return report
		}
		report.RecoveryActions = append(report.RecoveryActions, "restore the original machine identity or revoke and set up this installation again")
		return report
	}
	store, err := identity.Open(identity.Config{StateRoot: stateRoot})
	if err != nil {
		report.failure = err
		report.SetupState, report.IdentityState = "unavailable", "unavailable"
		if err == identity.ErrInvalidStore || err == identity.ErrKeyConflict {
			report.SetupState, report.IdentityState = "invalid", "invalid"
		}
		report.RecoveryActions = append(report.RecoveryActions, "repair ownership and permissions of the Paperboat state directory")
		return report
	}
	registration, err := store.Registration()
	if err != nil {
		report.failure = err
		report.SetupState, report.IdentityState = "unavailable", "unavailable"
		if err == identity.ErrInvalidStore || err == identity.ErrKeyConflict {
			report.SetupState, report.IdentityState = "invalid", "invalid"
			report.RecoveryActions = append(report.RecoveryActions, "revoke the invalid machine registration and run pb setup")
		} else {
			report.RecoveryActions = append(report.RecoveryActions, "check access to the machine registration before changing this installation")
		}
		return report
	}
	report.SetupState, report.IdentityState = "configured", "valid"
	report.MachineID, report.EnvironmentID = registration.MachineID, registration.EnvironmentID
	report.InstallationGeneration = registration.InstallationGeneration
	report.InboxPath = registration.InboxPath
	if err := inbox.ValidatePath(registration.InboxPath); err != nil {
		report.failure = errors.Join(report.failure, err)
		report.InboxState = "unsafe_or_unavailable"
		report.RecoveryActions = append(report.RecoveryActions, "run pb inbox set <absolute-path> with a private writable directory")
	} else {
		report.InboxState = "ready"
	}
	if control, err := store.MachineControl(time.Now().UTC(), time.Hour); err == nil {
		if control.ExpiresAt.Before(time.Now().UTC()) {
			report.CredentialState = "grace"
		} else {
			report.CredentialState = "valid"
		}
	} else if registration.MachineID != "" {
		report.failure = errors.Join(report.failure, err)
		report.CredentialState = "invalid_or_expired"
		report.RecoveryActions = append(report.RecoveryActions, "run pb pair to renew host authority")
	}
	if runtime.GOOS == "windows" {
		report.ConfigService = windowsConfigServiceStatus()
		if report.ConfigService == "invalid" {
			report.RecoveryActions = append(report.RecoveryActions, "repair the Windows config-sync service")
		}
	} else if home, homeErr := os.UserHomeDir(); homeErr == nil {
		definition := filepath.Join(home, ".config", "systemd", "user", "paperboat-runtime-config.service")
		if runtime.GOOS == "darwin" {
			definition = filepath.Join(home, "Library", "LaunchAgents", service.ConfigLabel+".plist")
		}
		if info, err := os.Lstat(definition); err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			report.ConfigService = localConfigServiceState()
		} else if err == nil {
			report.ConfigService = "invalid"
			report.RecoveryActions = append(report.RecoveryActions, "repair the config-sync service definition")
		} else if !errors.Is(err, os.ErrNotExist) {
			report.failure = errors.Join(report.failure, err)
			report.ConfigService = "unavailable"
			report.RecoveryActions = append(report.RecoveryActions, "check access to the config-sync service definition")
		}
	} else {
		report.failure = errors.Join(report.failure, homeErr)
		report.ConfigService = "unavailable"
	}
	inspectLocalRuntimeHealth(ctx, &report, stateRoot)
	return report
}

func localConfigServiceState() string {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if runtime.GOOS == "linux" {
		query := exec.CommandContext(ctx, "systemctl", "--user", "is-active", "paperboat-runtime-config.service")
		prepareConfigServiceQuery(query)
		output, err := query.Output()
		state := strings.TrimSpace(string(output))
		if err == nil && state == "active" {
			return "active"
		}
		if state == "inactive" || state == "failed" || state == "activating" || state == "deactivating" {
			return "installed_inactive"
		}
		return "unavailable"
	}
	if runtime.GOOS == "windows" {
		return windowsConfigServiceStatus()
	}
	account, err := user.Current()
	if err != nil {
		return "installed_unknown"
	}
	if err := exec.CommandContext(ctx, "launchctl", "print", "gui/"+account.Uid+"/"+service.ConfigLabel).Run(); err == nil {
		return "active"
	}
	return "installed_inactive"
}

func inspectLocalRuntimeHealth(parent context.Context, report *localDoctorReport, stateRoot string) {
	ctx, cancel := context.WithTimeout(parent, 4*time.Second)
	defer cancel()
	body, err := requestTunnelHostRuntime(ctx, stateRoot, "/healthz", 64<<10)
	if err != nil {
		report.failure = errors.Join(report.failure, err)
		report.HostRuntime = "unavailable · local runtime could not be reached"
		return
	}
	live, err := decodeTunnelDoctorHealth(bytes.NewReader(body), 64<<10)
	if err != nil || !live {
		if err != nil {
			report.failure = errors.Join(report.failure, err)
		} else {
			report.failure = errors.Join(report.failure, errLocalDoctorHealthInvalid)
		}
		report.HostRuntime = "unhealthy · invalid liveness response"
		return
	}
	report.HostRuntime = "ready"
	body, err = requestTunnelHostRuntime(ctx, stateRoot, tunnelHostDiagnosticsPath, tunnelHostDiagnosticsMaxBytes)
	if err != nil {
		report.failure = errors.Join(report.failure, err)
		report.WorkloadCounts = "unavailable · runtime diagnostics could not be read"
		return
	}
	diagnostics, err := decodeTunnelHostDiagnostics(body)
	if err != nil {
		report.failure = errors.Join(report.failure, err)
		report.WorkloadCounts = "unavailable · invalid runtime diagnostics"
		return
	}
	state := diagnostics.Health.Dimensions.Service
	if state.Status != "" && state.Status != "ready" {
		report.HostRuntime = string(state.Status) + " · " + state.Summary
		if state.RepairAction != "" {
			report.RecoveryActions = append(report.RecoveryActions, state.RepairAction)
		}
	}
	if diagnostics.Workloads == nil {
		report.WorkloadCounts = "unavailable · runtime did not provide workload counts"
		return
	}
	report.WorkloadCounts = "available"
	report.TrackedSessions = diagnostics.Workloads.Sessions
	report.ActiveProcesses = diagnostics.Workloads.Processes
	report.ActiveAttachments = diagnostics.Workloads.Attachments
	report.ActiveUploads = diagnostics.Workloads.Uploads
}

func doctorUserMachine(ctx context.Context, client *api.Client, machineID string) (api.UserMachine, error) {
	machines, err := client.ListUserMachines(ctx)
	if err != nil {
		return api.UserMachine{}, err
	}
	for _, machine := range machines {
		if machine.ID == machineID {
			return machine, nil
		}
	}
	return api.UserMachine{}, errors.New("resolved machine is missing from the account")
}

// doctorUnsupportedVersion returns the server's refusal of an out-of-date client (426 update_required
// or an incompatible protocol), whose cure is `pb update`, not a new sign-in.
func doctorUnsupportedVersion(err error) error {
	var refusal *api.APIError
	if errors.As(err, &refusal) && (refusal.Code == "update_required" || refusal.Code == "incompatible_client_version") {
		return refusal
	}
	var incompatible *api.ErrIncompatibleVersion
	if errors.As(err, &incompatible) {
		return incompatible
	}
	return nil
}
