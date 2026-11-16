//go:build windows

package hostservice

import (
	"context"
	"path/filepath"
	"sync"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/windowsopenssh"
)

// WindowsAuthorizedKeys owns the fixed, machine-wide PaperboatSshd
// authorization file. The enrolled owner can supply key material through the
// SID-restricted host-service pipe, but it can never select a filesystem path.
type WindowsAuthorizedKeys struct {
	mu        sync.Mutex // one reconcile at a time
	stateRoot string
}

func NewWindowsAuthorizedKeys(instance string) (*WindowsAuthorizedKeys, error) {
	root, err := hostinstall.WindowsInstanceRoot(instance)
	if err != nil {
		return nil, err
	}
	return newWindowsAuthorizedKeys(filepath.Join(root, "ssh"))
}

func newWindowsAuthorizedKeys(stateRoot string) (*WindowsAuthorizedKeys, error) {
	if !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot {
		return nil, ErrInvalidConfig
	}
	return &WindowsAuthorizedKeys{stateRoot: stateRoot}, nil
}

func (r *WindowsAuthorizedKeys) ReconcileAuthorizedKeys(ctx context.Context, keys []string, principal string) (bool, error) {
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
	return windowsopenssh.ReconcileAuthorizedKeysContext(ctx, r.stateRoot, keys, principal)
}
