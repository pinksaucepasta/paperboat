//go:build !windows

package main

import (
	"context"
	"errors"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	"github.com/pinksaucepasta/paperboat/cli/internal/localapi"
	"github.com/pinksaucepasta/paperboat/cli/internal/managedssh"
	"github.com/spf13/cobra"
)

// With no managed configuration and no daemon to write it, no command starts OpenSSH.
func TestManagedSSHCommandsRefuseWithoutConfiguration(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir()) // any OpenSSH start would fail with a launch error instead
	ctx := withHostDependencies(t.Context(), func(d *hostDependencies) {
		d.refreshInventory = func(context.Context) error { return localapi.ErrTransportUnavailable }
	})
	destination := managedssh.Destination{AliasDomain: "machines.example.test", Alias: "box", Host: "box.machines.example.test", Port: 22, User: "me"}
	command := &cobra.Command{}
	command.SetContext(ctx)
	check := func(name string, err error) {
		t.Helper()
		var refused commandPreparationFailure
		if !errors.As(err, &refused) || refused.step != prepareSSHConfiguration {
			t.Errorf("%s: err=%v, want the configuration refusal", name, err)
		}
	}
	check("ssh", executeManagedSSH(command, nil, api.UserMachine{}, destination, nil, false, nil))
	for _, tool := range []string{"scp", "sftp", "rsync"} {
		check(tool, verifyManagedToolLaunch(ctx, tool, []string{"f", "box:/x"}, destination, "me", nil))
	}
}
