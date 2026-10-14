package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/config"
	previewruntime "github.com/pinksaucepasta/paperboat/internal/hostruntime/preview"
	"github.com/pinksaucepasta/paperboat/internal/privatepreviewproxy"
	"github.com/spf13/cobra"
)

const accessMachineMaximumConnections = 128

var (
	errAccessMachineInvalid        = errors.New("invalid machine access request")
	accessMachineRuntimeForCommand = newProductionAccessMachineRuntime
)

type accessMachineRuntime interface {
	DialMachine(context.Context, string, int) (net.Conn, error)
	Close() error
}

type accessMachineResult struct {
	Schema        string `json:"schema"`
	Kind          string `json:"kind"`
	MachineID     string `json:"machine_id"`
	RemotePort    int    `json:"remote_port"`
	ListenAddress string `json:"listen_address"`
}

type productionAccessMachineRuntime struct {
	access *previewruntime.NativePrivateTCPAccess
	close  func() error
}

func (r *productionAccessMachineRuntime) DialMachine(ctx context.Context, machineID string, port int) (net.Conn, error) {
	return r.access.DialMachine(ctx, machineID, port)
}

func (r *productionAccessMachineRuntime) Close() error {
	if r == nil || r.close == nil {
		return nil
	}
	return r.close()
}

func newProductionAccessMachineRuntime(command *cobra.Command, selector string) (accessMachineRuntime, string, error) {
	commandContext := actionContext(command, []string{selector})
	commandContext.Context = command.Context()
	dependencies, err := buildDeps(commandContext)
	if err != nil {
		return nil, "", err
	}
	if dependencies.peerTunnel == nil {
		return nil, "", errors.New("Paperboat server is not configured; set server_url or use --server")
	}
	credential, err := dependencies.auth.Credential()
	if err != nil {
		_ = dependencies.peerTunnel.Close()
		if errors.Is(err, config.ErrNoCredentials) || errors.Is(err, config.ErrSecretNotFound) {
			return nil, "", errors.New("Paperboat sign-in credentials are unavailable; run the enrollment command from the Paperboat dashboard, then retry")
		}
		return nil, "", fmt.Errorf("load Paperboat sign-in credentials: %w", err)
	}
	client, err := newWorkspaceAPIClient(commandContext, dependencies.cfg.ServerURL, credential)
	if err != nil {
		_ = dependencies.peerTunnel.Close()
		return nil, "", err
	}
	machine, err := resolveUserMachine(command.Context(), client, selector)
	if err != nil {
		_ = dependencies.peerTunnel.Close()
		return nil, "", err
	}
	access, err := previewruntime.NewNativePrivateTCPAccess(previewruntime.NativePrivateTCPAccessConfig{
		Grants: client, DialSession: dependencies.peerTunnel.DialPrivateSession,
	})
	if err != nil {
		_ = dependencies.peerTunnel.Close()
		return nil, "", err
	}
	return &productionAccessMachineRuntime{access: access, close: dependencies.peerTunnel.Close}, machine.ID, nil
}

func accessMachineCobraCommandV1() *cobra.Command {
	command := &cobra.Command{
		Use:   "machine <machine>",
		Short: "Forward a local port to an authorized machine service",
		Long:  "Forward a loopback TCP port in this process's network namespace to one authorized machine service. The forward runs in the foreground; processes sharing the namespace can connect to it.",
		Args:  commandArgs(cobra.ExactArgs(1)),
		RunE:  runAccessMachine,
	}
	command.Flags().Int("port", 0, "remote machine TCP port")
	command.Flags().String("listen", "127.0.0.1:0", "literal-loopback listen address")
	tunnelJSONFlag(command)
	return command
}

func runAccessMachine(command *cobra.Command, args []string) error {
	port, _ := command.Flags().GetInt("port")
	if port < 1 || port > 65535 {
		return fmt.Errorf("%w: --port must be between 1 and 65535", errAccessMachineInvalid)
	}
	listen, _ := command.Flags().GetString("listen")
	listen, err := literalLoopbackAddress(listen)
	if err != nil {
		return fmt.Errorf("%w: --listen must be a literal loopback address", errAccessMachineInvalid)
	}
	runtime, machineID, err := accessMachineRuntimeForCommand(command, args[0])
	if err != nil {
		return err
	}
	proxy, err := privatepreviewproxy.Start(command.Context(), privatepreviewproxy.Config{
		ListenAddress: listen, MaximumConnections: accessMachineMaximumConnections,
		Dial: func(ctx context.Context) (io.ReadWriteCloser, error) {
			return runtime.DialMachine(ctx, machineID, port)
		},
	})
	if err != nil {
		return errors.Join(fmt.Errorf("open authorized access to %s port %d: %w", args[0], port, err), runtime.Close())
	}
	endpoint := strings.TrimPrefix(proxy.URL, "http://")
	if parsed, parseErr := url.Parse(proxy.URL); parseErr == nil && parsed.Host != "" {
		endpoint = parsed.Host
	}
	result := accessMachineResult{Schema: "paperboat.machine-access/v1", Kind: "machine_access", MachineID: machineID, RemotePort: port, ListenAddress: endpoint}
	human := fmt.Sprintf("Machine access to %s port %s is listening on %s", args[0], strconv.Itoa(port), endpoint)
	if err = tunnelOutput(command, result, human); err != nil {
		return errors.Join(err, proxy.Close(), runtime.Close())
	}
	return errors.Join(proxy.Wait(), proxy.Close(), runtime.Close())
}
