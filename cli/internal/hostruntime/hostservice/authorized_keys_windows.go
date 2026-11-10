//go:build windows

package hostservice

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/windowsopenssh"
)

// WindowsAuthorizedKeys owns the fixed, machine-wide PaperboatSshd
// authorization file. The enrolled owner can supply key material through the
// SID-restricted host-service pipe, but it can never select a filesystem path.
type WindowsAuthorizedKeys struct {
	mu                  sync.Mutex // one reconcile and its session duty at a time
	stateRoot, instance string
	// terminate ends the open managed SSH sessions of the removed keys (fingerprints; all of them for
	// windowsopenssh.AllSessions); a test replaces it.
	terminate func(instance, stateRoot string, removed []string) (windowsopenssh.TerminateResult, error)
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
	r.mu.Lock()
	defer r.mu.Unlock()
	changed, removed, err := windowsopenssh.ReconcileAuthorizedKeysRevoking(ctx, r.stateRoot, keys)
	if err != nil || r.instance == "" {
		return changed, err
	}
	// A key left: sshd checks keys only at login, so its open sessions end now (F5-10). The marker lists the
	// fingerprints still owed: the key file is already updated, so a failed attempt would otherwise be forgotten.
	marker := filepath.Join(r.stateRoot, "sessions-to-end")
	owed, err := owedFingerprints(marker, removed)
	if err != nil {
		return changed, fmt.Errorf("the key was removed but the duty to end its SSH sessions could not be recorded: %w", err)
	}
	if len(owed) == 0 {
		return changed, nil
	}
	result, err := r.terminate(r.instance, r.stateRoot, owed)
	if result.Fallback != "" {
		slog.Warn("managed SSH revoke ended every session, not only the removed keys' sessions", "reason", result.Fallback, "ended", result.Ended)
	}
	if err != nil {
		return changed, fmt.Errorf("the key was removed but open SSH sessions could not all be ended (retried at the next refresh): %w", err)
	}
	return changed, os.Remove(marker)
}

// owedFingerprints adds the newly removed keys to the marker's list and returns the whole list.
func owedFingerprints(marker string, removed []string) ([]string, error) {
	body, err := os.ReadFile(marker)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	owed := strings.Fields(string(body))
	if len(removed) == 0 {
		return owed, nil
	}
	owed = append(owed, removed...)
	slices.Sort(owed)
	owed = slices.Compact(owed)
	return owed, atomicfile.Write(marker, []byte(strings.Join(owed, "\n")+"\n"), atomicfile.Options{Mode: 0o600, OwnerUID: -1, OwnerGID: -1})
}
