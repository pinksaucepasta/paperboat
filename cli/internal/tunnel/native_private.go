package tunnel

import (
	"context"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/preview"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/clientauthority"
	"github.com/pinksaucepasta/paperboat/internal/peertransport/peerquic"
)

// DialPrivateSession shares the daemon's root-verified native runtime. Callers
// must obtain their exact resource grant before calling this network operation;
// generation is the machine installation that grant names, so the dial lists
// nothing (it listed every machine of the account per connection).
func (t *PeerTerminalTunnel) DialPrivateSession(ctx context.Context, machineID string, generation uint64) (preview.NativePrivateSession, error) {
	if t == nil || ctx == nil || machineID == "" || generation == 0 {
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

	// Native network bindings authenticate the remote peer, including a teammate's machine that
	// is shared into a team. Its endpoint certificate is private to the owner's account, so
	// loading it here would reject exactly the shared machines this path exists for.
	identity, err := t.authorities.ResolveLocal(ctx, clientauthority.Request{Store: t.config.Store, Client: client, Issuer: t.config.Issuer, AccountID: profile.Account.ID, CLIClientSessionID: profile.CLIClientSessionID, Now: t.config.Now().UTC()})
	if err != nil {
		return nil, err
	}
	runtime, consumed, err := t.acquireNativeRuntime(ctx, profile.Account.ID, profile.CLIClientSessionID, identity, nil)
	if !consumed {
		identity.Clear()
	}
	if err != nil {
		return nil, err
	}
	// Machine access carries authorized raw TCP streams. The preview class
	// negotiates HTTP/3 and is dispatched to a different host handler.
	return runtime.Dial(ctx, machineID, peerquic.ClassInteractive)
}
