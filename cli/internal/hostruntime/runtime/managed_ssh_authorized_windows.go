//go:build windows

package runtime

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostservice"
	hostruntimeservice "github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"golang.org/x/sys/windows"
)

type windowsAuthorizedKeysClient interface {
	ReconcileAuthorizedKeys(context.Context, []string) (bool, error)
}

var newWindowsAuthorizedKeysClient = func(timeout time.Duration) (windowsAuthorizedKeysClient, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		return nil, ErrProductionInvalid
	}
	instance, err := hostruntimeservice.WindowsUserInstance(user.User.Sid.String())
	if err != nil {
		return nil, err
	}
	path, err := hostservice.WindowsSocketPath(instance)
	if err != nil {
		return nil, err
	}
	return hostservice.NewClient(path, timeout)
}

func reconcilePlatformAuthorizedKeys(stateRoot string, _ uint32, keys []string) (bool, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		return false, ErrProductionInvalid
	}
	instance, err := hostruntimeservice.WindowsUserInstance(user.User.Sid.String())
	if err != nil {
		return false, ErrProductionInvalid
	}
	instanceRoot, err := hostinstall.WindowsInstanceRoot(instance)
	if err != nil {
		return false, ErrProductionInvalid
	}
	expectedRoot := filepath.Join(instanceRoot, "ssh")
	if !filepath.IsAbs(stateRoot) || filepath.Clean(stateRoot) != stateRoot || !strings.EqualFold(stateRoot, expectedRoot) {
		return false, ErrProductionInvalid
	}
	const timeout = 5 * time.Second
	client, err := newWindowsAuthorizedKeysClient(timeout)
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return client.ReconcileAuthorizedKeys(ctx, keys)
}
