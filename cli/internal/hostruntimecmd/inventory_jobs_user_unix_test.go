//go:build darwin || linux

package hostruntimecmd

import (
	"context"
	"os/user"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// A fresh Ubuntu container (no dbus-user-session) failed every pb install with
// "purge stage failed: final_inventory": `systemctl --user --machine=tester@ show` from root
// needs the account's D-Bus session bus. The user daemon is read as the account itself.
func TestUserDaemonUnitIsReadAsTheAccountWithoutItsSessionBus(t *testing.T) {
	account := &user.User{Uid: "1001", Gid: "1002", Username: "tester"}
	command, err := userSystemctl(context.Background(), account, "show", "--property=Id,MainPID", "paperboatd.service")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"/usr/bin/systemctl", "--user", "show", "--property=Id,MainPID", "paperboatd.service"}; !reflect.DeepEqual(command.Args, want) {
		t.Fatalf("args = %q, want %q", command.Args, want)
	}
	if slices.ContainsFunc(command.Args, func(arg string) bool { return strings.HasPrefix(arg, "--machine=") }) {
		t.Fatal("reaches the user manager through --machine, which needs the account's session bus")
	}
	if want := []string{"XDG_RUNTIME_DIR=/run/user/1001"}; !reflect.DeepEqual(command.Env, want) {
		t.Fatalf("env = %q, want only %q", command.Env, want)
	}
	credential := command.SysProcAttr.Credential
	if credential == nil || credential.Uid != 1001 || credential.Gid != 1002 {
		t.Fatalf("credential = %+v, want uid 1001 gid 1002", credential)
	}
}
