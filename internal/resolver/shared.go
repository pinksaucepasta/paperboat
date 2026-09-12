package resolver

import (
	"context"
	"errors"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

type sharedSessionClient interface {
	SharedTerminalConnectionDescriptor(context.Context, string) (api.ConnectionDescriptor, error)
}

// ResolveShared issues current exact-session authority without requiring a
// machine grant or looking up an owner's machine through the personal catalog.
func (r *APIResolver) ResolveShared(ctx context.Context, sessionID string) (ConnectInfo, error) {
	if err := r.validatePolicy(); err != nil {
		return ConnectInfo{}, err
	}
	client, ok := r.client.(sharedSessionClient)
	if !ok || sessionID == "" {
		return ConnectInfo{}, errors.New("shared terminal connection is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, r.readyTimeout)
	defer cancel()
	for {
		resp, err := client.SharedTerminalConnectionDescriptor(ctx, sessionID)
		if err != nil {
			return ConnectInfo{}, err
		}
		if err := terminalConnectionError(resp); err != nil {
			return ConnectInfo{}, err
		}
		if !resp.Connectable {
			interval := r.pollInterval
			if resp.RetryAfterSeconds > 0 {
				interval = time.Duration(resp.RetryAfterSeconds) * time.Second
			}
			if r.Progress != nil {
				r.Progress(resp.Status, resp.Reason, interval)
			}
			if err := r.wait(ctx, interval); err != nil {
				return ConnectInfo{}, err
			}
			continue
		}
		if resp.UserMachineID == "" || resp.MachineGeneration == 0 || resp.Terminal == nil || resp.Terminal.SessionID != sessionID || resp.FileTransfer != nil || len(resp.Capabilities) != 1 || resp.Capabilities[0] != "terminal" {
			return ConnectInfo{}, errors.New("server returned invalid shared session authority")
		}
		target := target{kind: targetUserMachine, id: resp.UserMachineID, generation: resp.MachineGeneration}
		if _, err := r.validateDescriptor(resp, target); err != nil {
			return ConnectInfo{}, err
		}
		terminal := &TerminalTarget{Protocol: resp.Terminal.Protocol, EnvironmentID: resp.Environment.EnvironmentID, QUICEndpoint: resp.Terminal.Endpoints.QUIC, WSSEndpoint: resp.Terminal.Endpoints.WSS, Auth: mapAuth(resp.Terminal.Auth), ThreadID: resp.Terminal.ThreadID, TerminalID: resp.Terminal.TerminalID, SessionID: sessionID, CWD: resp.Terminal.CWD, ReplayHistory: true}
		if !terminal.Shared() || terminal.Auth.ResourceID == "" {
			return ConnectInfo{}, errors.New("server returned invalid shared terminal role")
		}
		return ConnectInfo{TargetKind: targetUserMachine, ProjectID: resp.UserMachineID, Project: resp.Environment.DisplayName, ProjectState: resp.UserMachineState, MachineGeneration: resp.MachineGeneration, TunnelTarget: resp.Terminal.Endpoints.WSS, Terminal: terminal}, nil
	}
}
