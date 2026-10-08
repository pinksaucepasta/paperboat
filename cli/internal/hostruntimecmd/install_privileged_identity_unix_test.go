//go:build darwin || linux

package hostruntimecmd

import (
	"context"
	"os"
	"reflect"
	"testing"
)

func TestPrivilegedInstallCommandsSelectRootHomeAndKeepEnrolledUID(t *testing.T) {
	for _, operation := range []string{"install-supplied", "commit-supplied", "rollback-supplied", "manuals"} {
		t.Run(operation, func(t *testing.T) {
			arguments := []string{"__runtime-service", operation}
			if operation == "manuals" {
				arguments = []string{"--no-customization", "__man-pages", "--directory", "/usr/local/share/man"}
			}
			for _, uid := range []int{501, 0} {
				command := privilegedInstallCommand(context.Background(), uid, "/var/root", "501", "/protected/pb", arguments...)
				want := []string{"/usr/bin/sudo", "-H", "--", "/usr/bin/env", "PAPERBOAT_INVOKING_UID=501", "/protected/pb"}
				if uid == 0 {
					want = []string{"/usr/bin/env", "HOME=/var/root", "USER=root", "LOGNAME=root", "PAPERBOAT_INVOKING_UID=501", "/protected/pb"}
				}
				want = append(want, arguments...)
				if !reflect.DeepEqual(command.Args, want) {
					t.Fatalf("effective UID %d: args=%q want=%q", uid, command.Args, want)
				}
			}
		})
	}
}

func TestPrivilegedInstallDirectRootChildOverridesInheritedUserHome(t *testing.T) {
	t.Setenv("HOME", "/enrolled-home")
	t.Setenv("USER", "enrolled")
	t.Setenv("LOGNAME", "enrolled")
	command := privilegedInstallCommand(t.Context(), 0, "/var/root", "501", "/bin/sh", "-c", `printf '%s\n' "$HOME" "$USER" "$LOGNAME" "$PAPERBOAT_INVOKING_UID"`)
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != "/var/root\nroot\nroot\n501\n" {
		t.Fatalf("unexpected child identity: %q", output)
	}
	if os.Getenv("HOME") != "/enrolled-home" || os.Getenv("USER") != "enrolled" || os.Getenv("LOGNAME") != "enrolled" {
		t.Fatal("privileged child changed parent identity")
	}
}
