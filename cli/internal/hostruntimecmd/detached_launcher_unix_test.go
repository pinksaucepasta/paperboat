//go:build linux || darwin

package hostruntimecmd

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

// On Linux, sudo-rs kills a root step about 3 s after its terminal closes even with every signal ignored, and a
// killed sudo hung it up mid-rollback (device checks 11-27). The install, commit and rollback steps run as a
// transient systemd service instead, which finishes whatever happens to pb, sudo or the terminal; other steps and
// macOS keep the plain sudo child.
func TestInstallStepsRunAsATransientServiceOnLinux(t *testing.T) {
	fake := filepath.Join(t.TempDir(), "systemd-run")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := systemdRun
	t.Cleanup(func() { systemdRun = previous })
	systemdRun = fake
	for _, step := range []string{"install-supplied", "commit-supplied", "rollback-supplied", "inventory"} {
		command := privilegedInstallCommand(context.Background(), 1000, "/root", "1000", "/usr/local/bin/pb", "__runtime-service", step)
		detached := slices.Contains(command.Args, fake)
		want := runtime.GOOS == "linux" && step != "inventory"
		if detached != want {
			t.Fatalf("%s on %s: transient service = %v, want %v (%v)", step, runtime.GOOS, detached, want, command.Args)
		}
		if detached {
			for _, flag := range []string{"--wait", "--pipe", "--collect", "--property=KillMode=process", "HOME=/root"} {
				if !slices.Contains(command.Args, flag) {
					t.Fatalf("%s: launcher lacks %s: %v", step, flag, command.Args)
				}
			}
		}
	}
}
