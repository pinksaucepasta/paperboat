package daemoncmd

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	sessionauth "github.com/pinksaucepasta/paperboat/internal/auth"
	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/diagnosticlog"
	"github.com/pinksaucepasta/paperboat/internal/endpointbinary"
	helperconfig "github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/internal/hostruntimecmd"
	"github.com/pinksaucepasta/paperboat/internal/hostruntimeentry"
	"github.com/pinksaucepasta/paperboat/internal/httptransport"
	"github.com/pinksaucepasta/paperboat/internal/localdaemon"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/connectionmanager"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/transportmanager"
	"github.com/pinksaucepasta/paperboat/internal/tunnel"
	"github.com/pinksaucepasta/paperboat/internal/windowsopenssh"
	"github.com/spf13/cobra"
)

var platformUpdateProbeCommand func() *cobra.Command

// NewCommand exposes the persistent endpoint lifecycle beneath pb daemon.
func NewCommand() *cobra.Command {
	root := localDaemonCommand()
	root.Short = "Paperboat endpoint daemon"
	root.Hidden = false
	root.Version = buildinfo.Version
	root.PersistentFlags().String("config", "", "configuration file")
	root.PersistentFlags().String("server", "", "Paperboat server URL")
	root.AddCommand(hostRuntimeCommand())
	root.AddCommand(hostdRuntimeCommand())
	root.AddCommand(runtimeWorkerCommand())
	root.AddCommand(updatedRuntimeCommand())
	root.AddCommand(activatorRuntimeCommand())
	root.AddCommand(localDaemonRuntimeCommand())
	root.AddCommand(windowsSSHDServiceCommand())
	root.AddCommand(privilegedHostServiceCommand())
	root.AddCommand(configRuntimeCommand())
	if platformUpdateProbeCommand != nil {
		root.AddCommand(platformUpdateProbeCommand())
	}
	return root
}

type exitCodeError struct{ code int }

func (e exitCodeError) Error() string                            { return "" }
func (e exitCodeError) ExitCode() int                            { return e.code }
func commandArgs(args cobra.PositionalArgs) cobra.PositionalArgs { return args }

var currentLocalDaemonPaths = localdaemon.CurrentUserPaths

func cliExecutable() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	return endpointbinary.CLI(executable)
}

func hostRuntimeCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "__runtime-host",
		Hidden: true,
		Args:   commandArgs(cobra.NoArgs),
		RunE: func(command *cobra.Command, _ []string) error {
			code := hostruntimecmd.Execute(
				command.Context(), []string{"run"}, command.InOrStdin(), command.OutOrStdout(), command.ErrOrStderr(),
			)
			if code != 0 {
				return exitCodeError{code: code}
			}
			return nil
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}
}

func hostdRuntimeCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "__runtime-hostd",
		Hidden: true,
		Args:   commandArgs(cobra.NoArgs),
		RunE: func(command *cobra.Command, _ []string) error {
			code := hostruntimecmd.Execute(command.Context(), []string{"hostd"}, command.InOrStdin(), command.OutOrStdout(), command.ErrOrStderr())
			if code != 0 {
				return exitCodeError{code: code}
			}
			return nil
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}
}

func runtimeWorkerCommand() *cobra.Command {
	return &cobra.Command{
		Use:                "__runtime-worker",
		Hidden:             true,
		DisableFlagParsing: true,
		RunE: func(command *cobra.Command, args []string) error {
			code := hostruntimecmd.Execute(command.Context(), append([]string{"worker"}, args...), command.InOrStdin(), command.OutOrStdout(), command.ErrOrStderr())
			if code != 0 {
				return exitCodeError{code: code}
			}
			return nil
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}
}

func updatedRuntimeCommand() *cobra.Command {
	return &cobra.Command{Use: "__runtime-updated", Hidden: true, DisableFlagParsing: true, RunE: func(command *cobra.Command, args []string) error {
		code := hostruntimecmd.Execute(command.Context(), append([]string{"updated"}, args...), command.InOrStdin(), command.OutOrStdout(), command.ErrOrStderr())
		if code != 0 {
			return exitCodeError{code: code}
		}
		return nil
	}, SilenceUsage: true, SilenceErrors: true}
}

func activatorRuntimeCommand() *cobra.Command {
	return &cobra.Command{Use: "__runtime-activate", Hidden: true, DisableFlagParsing: true, RunE: func(command *cobra.Command, args []string) error {
		code := hostruntimecmd.Execute(command.Context(), append([]string{"activate"}, args...), command.InOrStdin(), command.OutOrStdout(), command.ErrOrStderr())
		if code != 0 {
			return exitCodeError{code: code}
		}
		return nil
	}, SilenceUsage: true, SilenceErrors: true}
}

func localDaemonRuntimeCommand() *cobra.Command {
	return &cobra.Command{Use: "__runtime-local-daemon", Hidden: true, DisableFlagParsing: true, RunE: func(command *cobra.Command, args []string) error {
		code := hostruntimecmd.Execute(command.Context(), append([]string{"local-daemon-service"}, args...), command.InOrStdin(), command.OutOrStdout(), command.ErrOrStderr())
		if code != 0 {
			return exitCodeError{code: code}
		}
		return nil
	}, SilenceUsage: true, SilenceErrors: true}
}

func windowsSSHDServiceCommand() *cobra.Command {
	command := &cobra.Command{
		Use:    "__windows-sshd-service",
		Hidden: true,
		Args:   commandArgs(cobra.NoArgs),
		RunE: func(command *cobra.Command, _ []string) error {
			sshdPath, err := command.Flags().GetString("sshd")
			if err != nil {
				return err
			}
			configPath, err := command.Flags().GetString("config")
			if err != nil {
				return err
			}
			return windowsopenssh.RunServiceHost(sshdPath, configPath)
		},
		SilenceUsage: true, SilenceErrors: true,
	}
	command.Flags().String("sshd", "", "managed sshd executable")
	command.Flags().String("config", "", "managed sshd configuration")
	_ = command.MarkFlagRequired("sshd")
	_ = command.MarkFlagRequired("config")
	return command
}

func localDaemonCommand() *cobra.Command {
	return &cobra.Command{
		Use:    "daemon",
		Hidden: true,
		Args:   commandArgs(cobra.NoArgs),
		RunE: func(command *cobra.Command, _ []string) error {
			cfg, err := config.Load(configPathFlag(command))
			if err != nil {
				return err
			}
			if server, _ := command.Flags().GetString("server"); strings.TrimSpace(server) != "" {
				cfg.ServerURL, err = config.NormalizeServerURL(server)
				if err != nil {
					return err
				}
			}
			if strings.TrimSpace(cfg.ServerURL) == "" {
				return errors.New("Paperboat server is not configured")
			}
			authSource, err := sessionauth.NewSource(cfg)
			if err != nil {
				return err
			}
			authSource = authSource.WithContext(command.Context())
			paths, err := currentLocalDaemonPaths()
			if err != nil {
				return err
			}
			source := &localdaemon.AuthenticatedMachineSource{ServerURL: cfg.ServerURL, Auth: authSource}
			source.ReportPeerApprovalSignerUnavailable = localdaemon.RateLimitedPeerApprovalReporter(time.Now, time.Minute, func(issue localdaemon.PeerApprovalSignerUnavailableError) {
				diagnosticlog.TryInfo("peer enrollment signer unavailable", "reason", "verifier_only", "pending_requests", issue.PendingRequests)
			})
			source.SourceMachineID, err = configuredMachineID()
			if err != nil {
				return err
			}
			var managedConfig *localdaemon.ManagedSSHConfig
			if store, storeErr := config.ProfileStoreFor(cfg); storeErr == nil {
				if profile, profileErr := store.Load(cfg.ServerURL); profileErr == nil {
					source.AutoApprovePeerEnrollments = func(ctx context.Context, client *api.Client, machines []api.UserMachine) error {
						return localdaemon.ApproveOwnedPeerEnrollments(ctx, store, profile, client, machines)
					}
					executable, executableErr := cliExecutable()
					if executableErr != nil {
						return executableErr
					}
					home, homeErr := os.UserHomeDir()
					if homeErr != nil {
						return homeErr
					}
					managedConfig = &localdaemon.ManagedSSHConfig{ServerURL: cfg.ServerURL, Auth: authSource, Store: store, CLIClientSessionID: profile.CLIClientSessionID, Home: home, RuntimeDirectory: paths.RuntimeRoot, Executable: executable, OwnerUID: uint32(os.Geteuid()), InheritedAgentSocket: os.Getenv("SSH_AUTH_SOCK")}
				}
			}
			store, err := config.ProfileStoreFor(cfg)
			if err != nil {
				return err
			}
			transportConfig := httptransport.DevelopmentConfig()
			if transportConfig.TLSConfig == nil {
				transportConfig.TLSConfig = &tls.Config{}
			}
			transportConfig.TLSConfig.MinVersion = tls.VersionTLS13
			peerHTTPTransport, err := httptransport.New(transportConfig)
			if err != nil {
				return err
			}
			source.HTTPClient = &http.Client{Transport: peerHTTPTransport}
			peerManager, err := transportmanager.New()
			if err != nil {
				return err
			}
			transportMode, err := tunnel.ParseTerminalTransport(cfg.Connect.TerminalTransport)
			if err != nil {
				_ = peerManager.Close()
				return err
			}
			peerTunnel, err := tunnel.NewPeerTerminalTunnel(tunnel.PeerTerminalConfig{Issuer: cfg.ServerURL, Store: store, Auth: authSource, TLS: transportConfig.TLSConfig, HTTPClient: &http.Client{Transport: peerHTTPTransport}, OutputQueueChunks: cfg.Connect.TerminalOutputQueueChunks, Mode: peerConnectionMode(transportMode), PublishLocalStatus: true, TransportManager: peerManager, Race: peerRacePolicy()})
			if err != nil {
				_ = peerManager.Close()
				return err
			}
			if err := peerTunnel.Start(command.Context()); err != nil {
				_ = peerManager.Close()
				return err
			}
			defer peerTunnel.Close()
			fileTransfers, err := localdaemon.NewFileTransferBroker(peerTunnel)
			if err != nil {
				return err
			}
			return localdaemon.Run(command.Context(), localdaemon.DaemonConfig{
				Paths: paths, Source: source, ManagedSSH: managedConfig, IssuePeerStream: source.IssuePeerStream,
				OwnerUID: os.Geteuid(), OwnerGID: os.Getegid(),
				TransportManager: peerManager, OpenPeerStream: localdaemon.TunnelPeerStreamOpener(peerTunnel), ProbePeer: localdaemon.TunnelPeerProbe(peerTunnel), FileTransfers: fileTransfers, InvalidatePeerAuthority: peerTunnel.InvalidateMachine, WarmPeerMetadata: peerTunnel.WarmMachines,
			})
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}
}

func privilegedHostServiceCommand() *cobra.Command {
	return &cobra.Command{
		Use:                "__runtime-host-service",
		Hidden:             true,
		DisableFlagParsing: true,
		RunE: func(command *cobra.Command, args []string) error {
			code := hostruntimecmd.ExecuteHostService(command.Context(), args, command.ErrOrStderr())
			if code != 0 {
				return exitCodeError{code: code}
			}
			return nil
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}
}

func configRuntimeCommand() *cobra.Command {
	command := &cobra.Command{
		Use:    "__runtime-config",
		Hidden: true,
		Args:   commandArgs(cobra.NoArgs),
		RunE: func(command *cobra.Command, _ []string) error {
			stateRoot, err := command.Flags().GetString("state-root")
			if err != nil {
				return err
			}
			if stateRoot == "" {
				stateRoot = os.Getenv("PAPERBOAT_RUNTIME_STATE_ROOT")
			}
			if stateRoot == "" {
				stateRoot, err = helperconfig.DefaultStateRoot(os.Getenv)
				if err != nil {
					return err
				}
			}
			handled, err := enterWindowsConfigService(stateRoot)
			if err != nil {
				return err
			}
			if handled {
				return nil
			}
			store, err := identity.Open(identity.Config{StateRoot: stateRoot})
			if err != nil {
				return fmt.Errorf("open machine identity: %w", err)
			}
			registration, err := store.Registration()
			if err != nil {
				return fmt.Errorf("load machine registration: %w", err)
			}
			homeRoot, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			chezmoi := strings.TrimSpace(os.Getenv("PAPERBOAT_CHEZMOI_PATH"))
			if chezmoi == "" {
				chezmoi = defaultChezmoiPath()
			}
			hosts := []string{"github.com"}
			if raw := strings.TrimSpace(os.Getenv("PAPERBOAT_CONFIG_REPOSITORY_HOSTS")); raw != "" {
				hosts = strings.Split(raw, ",")
			}
			return hostruntimeentry.RunConfigWorker(command.Context(), hostruntimeentry.ConfigWorkerConfig{
				ControlURL: registration.ServerURL, StateRoot: stateRoot, HomeRoot: filepath.Clean(homeRoot),
				ChezmoiBinary: chezmoi, RepositoryHosts: hosts,
			})
		},
		SilenceUsage: true, SilenceErrors: true,
	}
	command.Flags().String("state-root", "", "runtime state directory")
	return command
}

func peerConnectionMode(mode tunnel.TerminalTransport) connectionmanager.Mode {
	switch mode {
	case tunnel.TerminalTransportDirect:
		return connectionmanager.ModeDirectQUIC
	case tunnel.TerminalTransportRelayQUIC:
		return connectionmanager.ModeRelayQUIC
	case tunnel.TerminalTransportRelayWSS:
		return connectionmanager.ModeWSS
	case tunnel.TerminalTransportRelay:
		return connectionmanager.ModeRelayRace
	default:
		return connectionmanager.ModeAuto
	}
}

func peerRacePolicy() connectionmanager.Config {
	return connectionmanager.Config{
		RelayDelay:     time.Duration(config.PeerRelayPreferenceMilliseconds) * time.Millisecond,
		WSSDelay:       time.Duration(config.PeerWSSStartMilliseconds) * time.Millisecond,
		ConnectTimeout: time.Duration(config.PeerConnectTimeoutMilliseconds) * time.Millisecond,
	}
}

func runtimeIdentityStore() (*identity.Store, error) {
	stateRoot := os.Getenv("PAPERBOAT_RUNTIME_STATE_ROOT")
	var err error
	if stateRoot == "" {
		stateRoot, err = helperconfig.DefaultStateRoot(os.Getenv)
		if err != nil {
			return nil, err
		}
	}
	return identity.Open(identity.Config{StateRoot: stateRoot})
}

func configuredMachineID() (string, error) {
	store, err := runtimeIdentityStore()
	if err != nil {
		return "", err
	}
	registration, err := store.Registration()
	if err != nil || registration.MachineID == "" {
		return "", errors.New("run `pb setup` to configure this machine")
	}
	return registration.MachineID, nil
}

func configPathFlag(command *cobra.Command) string {
	value, _ := command.Flags().GetString("config")
	return value
}
