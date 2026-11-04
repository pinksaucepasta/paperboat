//go:build !windows

package hostruntimecmd

import (
	"runtime"
	"testing"
)

func TestSudoHomeIsDarwinOnly(t *testing.T) {
	got := sudoHome()
	if runtime.GOOS == "darwin" {
		if len(got) != 1 || got[0] != "-H" {
			t.Fatalf("darwin sudoHome = %v", got)
		}
		return
	}
	if len(got) != 0 {
		t.Fatalf("sudoHome on %s = %v, want none so Linux launchers are unchanged", runtime.GOOS, got)
	}
}

func TestSudoCommandKeepsLinuxArgv(t *testing.T) {
	command := sudoCommand(t.Context(), "--", "/usr/bin/env", "X=1")
	want := []string{"/usr/bin/sudo", "--", "/usr/bin/env", "X=1"}
	if runtime.GOOS == "darwin" {
		want = []string{"/usr/bin/sudo", "-H", "--", "/usr/bin/env", "X=1"}
	}
	if len(command.Args) != len(want) {
		t.Fatalf("args = %v, want %v", command.Args, want)
	}
	for i := range want {
		if command.Args[i] != want[i] {
			t.Fatalf("args = %v, want %v", command.Args, want)
		}
	}
}
