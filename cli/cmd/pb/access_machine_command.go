package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/daemonrpc"
	daemonpb "github.com/pinksaucepasta/paperboat/internal/daemonrpc/proto"
	"github.com/pinksaucepasta/paperboat/internal/privatepreviewproxy"
	"github.com/pinksaucepasta/paperboat/internal/resolver"
	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

// machineLocator is the local daemon's view of the machines it reaches. The daemon owns the
// machine's native peer identity, so a second process must not open its own peer session.
type machineLocator interface {
	ResolveMachine(context.Context, string) (*daemonpb.MachineAddress, error)
	Close() error
}

var newAccessMachineLocator = func(ctx context.Context) (machineLocator, error) {
	return daemonrpc.NewClient(ctx, "")
}

// daemonAccessMachineRuntime reaches a machine service through the daemon's assigned-address
// forward, which carries the same per-connection exact-port grant as `pb access` itself.
type daemonAccessMachineRuntime struct {
	locator machineLocator
}

func (r *daemonAccessMachineRuntime) DialMachine(ctx context.Context, machineID string, port int) (net.Conn, error) {
	address, err := r.locator.ResolveMachine(ctx, machineID)
	if err != nil {
		return nil, err
	}
	var dialer net.Dialer
	return dialer.DialContext(ctx, "tcp", net.JoinHostPort(address.GetAssignedIp(), strconv.Itoa(port)))
}

func (r *daemonAccessMachineRuntime) Close() error { return r.locator.Close() }

func newProductionAccessMachineRuntime(command *cobra.Command, selector string) (accessMachineRuntime, string, error) {
	commandContext := actionContext(command, []string{selector})
	commandContext.Context = command.Context()
	dependencies, err := buildDeps(commandContext)
	if err != nil {
		return nil, "", err
	}
	if dependencies.cfg.ServerURL == "" {
		return nil, "", errors.New("Paperboat server is not configured; set server_url or use --server")
	}
	_, err = dependencies.auth.Credential()
	if err != nil {
		if errors.Is(err, config.ErrNoCredentials) || errors.Is(err, config.ErrSecretNotFound) {
			return nil, "", errors.New("Paperboat sign-in credentials are unavailable; run the enrollment command from the Paperboat dashboard, then retry")
		}
		return nil, "", fmt.Errorf("load Paperboat sign-in credentials: %w", err)
	}
	client, err := newWorkspaceAPIClient(commandContext, dependencies.cfg.ServerURL, dependencies.auth)
	if err != nil {
		return nil, "", err
	}
	machine, err := resolveUserMachine(command.Context(), client, selector)
	if err != nil {
		return nil, "", err
	}
	port, _ := command.Flags().GetInt("port")
	locator, err := newAccessMachineLocator(command.Context())
	if err != nil {
		return nil, "", fmt.Errorf("reach the local Paperboat daemon: %w; run `pb service start` and retry", err)
	}
	if err := waitForMachineService(command.Context(), locator, machine.ID, port); err != nil {
		return nil, "", errors.Join(err, locator.Close())
	}
	return &daemonAccessMachineRuntime{locator: locator}, machine.ID, nil
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
	_ = command.MarkFlagRequired("port")
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

// A just-started service is reachable only once the daemon publishes its forward. Wait on the
// daemon's own listing of the machine; a machine it does not list is offline or not approved.
func waitForMachineService(ctx context.Context, locator machineLocator, machine string, port int) error {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(config.PeerConnectTimeoutMilliseconds)*time.Millisecond)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		address, err := locator.ResolveMachine(ctx, machine)
		switch {
		case status.Code(err) == codes.NotFound:
			return fmt.Errorf("%w: %q", resolver.ErrMachineNotFound, machine)
		case err != nil && ctx.Err() == nil:
			return fmt.Errorf("read the machine from the local daemon: %w", err)
		case err == nil && slices.Contains(address.GetForwardedPorts(), int32(port)):
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for machine service on port %d: start the service and check automatic discovery with pb doctor: %w", port, ctx.Err())
		case <-ticker.C:
		}
	}
}
