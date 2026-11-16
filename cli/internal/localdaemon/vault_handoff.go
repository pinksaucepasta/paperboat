//go:build darwin || linux

package localdaemon

import (
	"context"
	"errors"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

// vaultHandoffWindow bounds how long a replacement waits for the new daemon's socket to take the
// custody; the daemon binds it before its first refresh, so this is seconds in practice.
const vaultHandoffWindow = 20 * time.Second

// handOverVaultCustody keeps unlocked ENV vaults unlocked across a daemon replacement (pb update,
// pb install, pb login binding) where no OS keyring holds them: restart runs between an export from
// the old daemon (which keeps no copy) and an import into the new one, held only in this process's
// memory and sent only over the owner-only socket to a listener run by this user. A failed handoff
// never fails the replacement: the new daemon is locked and `pb env unlock` says so.
func handOverVaultCustody(ctx context.Context, daemon func(context.Context) (*localapi.Client, error), restart func() error) error {
	var handoff localapi.VaultCustodyHandoff
	if client, err := daemon(ctx); err == nil && client != nil {
		handoff, _ = client.VaultCustodyHandoff(ctx, localapi.VaultCustodyHandoff{Operation: "export"})
	}
	defer handoff.Clear()
	if err := restart(); err != nil || len(handoff.Entries) == 0 {
		return err
	}
	importCtx, cancel := context.WithTimeout(ctx, vaultHandoffWindow)
	defer cancel()
	for {
		client, err := daemon(importCtx)
		if err == nil && client != nil {
			if _, err = client.VaultCustodyHandoff(importCtx, handoff); err == nil || !errors.Is(err, localapi.ErrTransportUnavailable) {
				return nil
			}
		}
		select {
		case <-importCtx.Done():
			return nil
		case <-time.After(200 * time.Millisecond):
		}
	}
}
