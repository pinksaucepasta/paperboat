//go:build darwin || linux

package hostruntimecmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// pbvm2 (Ubuntu 24.04, passwordless sudo) failed `pb pair`: its sudoers has a
// stock `(ALL:ALL) ALL` rule before `NOPASSWD: ALL`, so `sudo -n true` works but
// `sudo -v` still demands a password.
func TestApproveAdministratorSkipsApprovalWhenSudoIsPasswordless(t *testing.T) {
	fake := filepath.Join(t.TempDir(), "sudo")
	script := "#!/bin/sh\n[ \"$1\" = -n ] && exit 0\necho 'sudo: a password is required' >&2\nexit 1\n"
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	previous := sudoExecutable
	sudoExecutable = fake
	t.Cleanup(func() { sudoExecutable = previous })
	var stderr bytes.Buffer
	if err := approveAdministrator(t.Context(), bytes.NewReader(nil), &stderr); err != nil || stderr.Len() != 0 {
		t.Fatalf("passwordless sudo must not require approval: err=%v stderr=%q", err, stderr.String())
	}
}

func TestApproveAdministratorStillRequiresApprovalWhenSudoNeedsAPassword(t *testing.T) {
	fake := filepath.Join(t.TempDir(), "sudo")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	previous := sudoExecutable
	sudoExecutable = fake
	t.Cleanup(func() { sudoExecutable = previous })
	if err := approveAdministrator(t.Context(), bytes.NewReader(nil), &bytes.Buffer{}); err == nil {
		t.Fatal("a refused sudo must fail administrator approval")
	}
}
