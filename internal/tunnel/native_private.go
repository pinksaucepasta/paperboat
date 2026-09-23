package tunnel

import (
	"context"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/preview"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/clientauthority"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
)

// DialPrivateSession shares the daemon's root-verified native runtime. Callers
// must obtain their exact resource grant before calling this network operation.
func (t *PeerTerminalTunnel) DialPrivateSession(ctx context.Context, machineID string) (preview.NativePrivateSession, error) {
	if t == nil || ctx == nil || machineID == "" {
		return nil, ErrPeerTerminalInvalid
	}
	credential, err := t.config.Auth.Credential()
	if err != nil {
		return nil, err
	}
	profile, err := t.config.Store.Load(t.config.Issuer)
	if err != nil {
		return nil, err
	}
	client := api.New(t.config.Issuer, credential, t.config.HTTPClient)
	machines, err := client.ListUserMachines(ctx)
	if err != nil {
		return nil, err
	}
	var generation uint64
	for _, machine := range machines {
		if machine.ID == machineID && machine.InstallationGeneration > 0 && machine.State != "revoked" && machine.State != "deleted" {
			generation = uint64(machine.InstallationGeneration)
			break
		}
	}

	if generation == 0 {
		devices, listErr := client.DeviceServices(ctx)
		if listErr != nil {
			return nil, listErr
		}
		for _, device := range devices {
			if device.MachineID == machineID && device.InstallationGeneration > 0 && device.ExpiresAt.After(t.config.Now()) {
				generation = uint64(device.InstallationGeneration)
				break
			}
		}
	}
	if generation == 0 {
		return nil, ErrPeerTerminalInvalid
	}

	identity, err := t.authorities.Resolve(ctx, clientauthority.Request{Store: t.config.Store, Client: client, Issuer: t.config.Issuer, AccountID: profile.Account.ID, CLIClientSessionID: profile.CLIClientSessionID, MachineID: machineID, MachineGeneration: generation, Now: t.config.Now().UTC()})
	if err != nil {
		return nil, err
	}
	runtime, consumed, err := t.acquireNativeRuntime(ctx, profile.Account.ID, profile.CLIClientSessionID, identity)
	if !consumed {
		identity.Clear()
	}
	if err != nil {
		return nil, err
	}
	// Device access carries authorized raw TCP streams. The preview class
	// negotiates HTTP/3 and is dispatched to a different host handler.
	return runtime.Dial(ctx, machineID, peerquic.ClassInteractive)
}
