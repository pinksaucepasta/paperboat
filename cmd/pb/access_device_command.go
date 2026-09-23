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

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	previewruntime "github.com/pinksaucepasta/paperboat/internal/hostruntime/preview"
	"github.com/pinksaucepasta/paperboat/internal/privatepreviewproxy"
	"github.com/spf13/cobra"
)

const accessDeviceMaximumConnections = 128

var (
	errAccessDeviceInvalid        = errors.New("invalid device access request")
	accessDeviceRuntimeForCommand = newProductionAccessDeviceRuntime
)

type accessDeviceRuntime interface {
	DialDevice(context.Context, string, int) (net.Conn, error)
	Close() error
}

type accessDeviceResult struct {
	Schema        string `json:"schema"`
	Kind          string `json:"kind"`
	MachineID     string `json:"machine_id"`
	RemotePort    int    `json:"remote_port"`
	ListenAddress string `json:"listen_address"`
}

type productionAccessDeviceRuntime struct {
	access *previewruntime.NativePrivateTCPAccess
	close  func() error
}

func (r *productionAccessDeviceRuntime) DialDevice(ctx context.Context, machineID string, port int) (net.Conn, error) {
	return r.access.DialDevice(ctx, machineID, port)
}

func (r *productionAccessDeviceRuntime) Close() error {
	if r == nil || r.close == nil {
		return nil
	}
	return r.close()
}

func newProductionAccessDeviceRuntime(command *cobra.Command, selector string) (accessDeviceRuntime, string, error) {
	commandContext := actionContext(command, []string{selector})
	commandContext.Context = command.Context()
	dependencies, err := buildDeps(commandContext)
	if err != nil {
		return nil, "", err
	}
	if dependencies.hostedTransferKeys == nil {
		return nil, "", errors.New("Paperboat server is not configured; set server_url or use --server")
	}
	credential, err := dependencies.auth.Credential()
	if err != nil {
		_ = dependencies.hostedTransferKeys.Close()
		if errors.Is(err, config.ErrNoCredentials) || errors.Is(err, config.ErrSecretNotFound) {
			return nil, "", errors.New("Paperboat sign-in credentials are unavailable; run the enrollment command from the Paperboat dashboard, then retry")
		}
		return nil, "", fmt.Errorf("load Paperboat sign-in credentials: %w", err)
	}
	client := api.New(dependencies.cfg.ServerURL, credential, nil)
	machine, err := resolveUserMachine(command.Context(), client, selector)
	if err != nil {
		_ = dependencies.hostedTransferKeys.Close()
		return nil, "", err
	}
	access, err := previewruntime.NewNativePrivateTCPAccess(previewruntime.NativePrivateTCPAccessConfig{
		Grants: client, DialSession: dependencies.hostedTransferKeys.DialPrivateSession,
	})
	if err != nil {
		_ = dependencies.hostedTransferKeys.Close()
		return nil, "", err
	}
	return &productionAccessDeviceRuntime{access: access, close: dependencies.hostedTransferKeys.Close}, machine.ID, nil
}

func accessDeviceCobraCommandV1() *cobra.Command {
	command := &cobra.Command{
		Use:   "device <machine>",
		Short: "Forward a local port to an authorized device service",
		Long:  "Forward a loopback TCP port in this process's network namespace to one authorized device service. The forward runs in the foreground; processes sharing the namespace can connect to it.",
		Args:  commandArgs(cobra.ExactArgs(1)),
		RunE:  runAccessDevice,
	}
	command.Flags().Int("port", 0, "remote device TCP port")
	command.Flags().String("listen", "127.0.0.1:0", "literal-loopback listen address")
	tunnelJSONFlag(command)
	return command
}

func runAccessDevice(command *cobra.Command, args []string) error {
	port, _ := command.Flags().GetInt("port")
	if port < 1 || port > 65535 {
		return fmt.Errorf("%w: --port must be between 1 and 65535", errAccessDeviceInvalid)
	}
	listen, _ := command.Flags().GetString("listen")
	listen, err := literalLoopbackAddress(listen)
	if err != nil {
		return fmt.Errorf("%w: --listen must be a literal loopback address", errAccessDeviceInvalid)
	}
	runtime, machineID, err := accessDeviceRuntimeForCommand(command, args[0])
	if err != nil {
		return err
	}
	proxy, err := privatepreviewproxy.Start(command.Context(), privatepreviewproxy.Config{
		ListenAddress: listen, MaximumConnections: accessDeviceMaximumConnections,
		Dial: func(ctx context.Context) (io.ReadWriteCloser, error) { return runtime.DialDevice(ctx, machineID, port) },
	})
	if err != nil {
		return errors.Join(fmt.Errorf("open authorized access to %s port %d: %w", args[0], port, err), runtime.Close())
	}
	endpoint := strings.TrimPrefix(proxy.URL, "http://")
	if parsed, parseErr := url.Parse(proxy.URL); parseErr == nil && parsed.Host != "" {
		endpoint = parsed.Host
	}
	result := accessDeviceResult{Schema: "paperboat.device-access/v1", Kind: "device_access", MachineID: machineID, RemotePort: port, ListenAddress: endpoint}
	human := fmt.Sprintf("Device access to %s port %s is listening on %s", args[0], strconv.Itoa(port), endpoint)
	if err = tunnelOutput(command, result, human); err != nil {
		return errors.Join(err, proxy.Close(), runtime.Close())
	}
	return errors.Join(proxy.Wait(), proxy.Close(), runtime.Close())
}
