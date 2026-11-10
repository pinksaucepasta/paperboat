//go:build windows

package hostservice

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/windowsopenssh"
)

// WindowsAuthorizedKeys owns the fixed, machine-wide PaperboatSshd
// authorization file. The enrolled owner can supply key material through the
// SID-restricted host-service pipe, but it can never select a filesystem path.
type WindowsAuthorizedKeys struct {
	stateRoot, instance string
	// terminate ends the instance's open managed SSH sessions; a test replaces it.
	terminate func(instance string) error
}

func NewWindowsAuthorizedKeys(instance string) (*WindowsAuthorizedKeys, error) {
	root, err := hostinstall.WindowsInstanceRoot(instance)
	if err != nil {
		return nil, err
	}
	keys, err := newWindowsAuthorizedKeys(filepath.Join(root, "ssh"))
	if err == nil {
		keys.instance = instance
	}
	return keys, err
}

func newWindowsAuthorizedKeys(stateRoot string) (*WindowsAuthorizedKeys, error) {
	if !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot {
		return nil, ErrInvalidConfig
	}
	return &WindowsAuthorizedKeys{stateRoot: stateRoot, terminate: windowsopenssh.TerminateManagedSessions}, nil
}

func (r *WindowsAuthorizedKeys) ReconcileAuthorizedKeys(ctx context.Context, keys []string) (bool, error) {
	if r == nil || ctx == nil {
		return false, ErrInvalidConfig
	}
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	default:
	}
	changed, removed, err := windowsopenssh.ReconcileAuthorizedKeysRevoking(ctx, r.stateRoot, keys)
	if err != nil || r.instance == "" {
		return changed, err
	}
	// A key left: sshd checks keys only at login, so the open sessions end now (F5-10). The marker keeps the duty
	// across a failed attempt: the key file is already updated, so the next reconcile would not see a removal.
	marker := filepath.Join(r.stateRoot, "sessions-to-end")
	if removed {
		if err := os.WriteFile(marker, nil, 0o600); err != nil {
			return changed, fmt.Errorf("the key was removed but the duty to end its SSH sessions could not be recorded: %w", err)
		}
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		return changed, nil
	}
	if err := r.terminate(r.instance); err != nil {
		return changed, fmt.Errorf("the key was removed but open SSH sessions could not all be ended (retried at the next refresh): %w", err)
	}
	return changed, os.Remove(marker)
}
