//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/windows/elevation"
	"github.com/pinksaucepasta/paperboat/shared/supportref"
	"github.com/spf13/cobra"
	"golang.org/x/sys/windows"
)

// freshEnrollmentRuntimePurge performs the same purge as `service purge`, but
// returns the purge's own failure so the reset step keeps its cause. The
// elevated child's curated message is also written to the command's stderr,
// as the exit-code path did before.
func freshEnrollmentRuntimePurge(command *cobra.Command) error {
	err := purgeFreshEnrollmentRuntime(command.Context())
	var remote *elevation.RemoteError
	if errors.As(err, &remote) {
		if _, writeErr := fmt.Fprintln(command.ErrOrStderr(), "pb: "+remote.Message); writeErr != nil {
			return errors.Join(err, writeErr)
		}
	}
	return err
}

func purgeFreshEnrollmentRuntime(ctx context.Context) error {
	if ctx == nil {
		return errors.New("runtime purge requires a context")
	}
	if supportref.FromContext(ctx) == "" {
		ctx = supportref.WithContext(ctx, supportref.New())
	}
	if elevation.IsCurrentProcessElevated() {
		ownerSID, err := currentProcessOwnerSID()
		if err != nil {
			return err
		}
		return hostinstall.PurgeForReset(ctx, ownerSID)
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	return elevation.RunRuntimeService(ctx, executable, elevation.ActionResetPurge, nil)
}

func currentProcessOwnerSID() (string, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		return "", errors.New("resolve current Windows user")
	}
	return user.User.Sid.String(), nil
}

func existingFreshEnrollmentExecutable() (string, error) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return "", err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		return "", errors.New("resolve current Windows user")
	}
	instance, err := hostinstall.WindowsInstanceForSID(user.User.Sid.String())
	if err != nil {
		return "", err
	}
	layout, err := hostinstall.WindowsLayoutForInstance(instance)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(layout.Binary); errors.Is(err, os.ErrNotExist) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	if _, err := installsource.Inspect(layout.Binary, "resume", installsource.Custom); err != nil {
		return "", err
	}
	return layout.Binary, nil
}
