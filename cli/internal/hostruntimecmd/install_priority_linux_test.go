//go:build linux

package hostruntimecmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestSuppliedInstallerRunsBelowInteractiveIOPriority(t *testing.T) {
	if os.Getenv("PB_TEST_INSTALL_PRIORITY") == "1" {
		priority, _, errno := unix.Syscall(unix.SYS_IOPRIO_GET, 1, 0, 0)
		nice, err := unix.Getpriority(unix.PRIO_PROCESS, 0)
		if errno != 0 || err != nil || priority>>13 != 3 || nice > 10 {
			fmt.Printf("wrong installer scheduling: io_class=%d nice=%d errno=%v error=%v", priority>>13, 20-nice, errno, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	command := privilegedInstallCommand(context.WithValue(t.Context(), yieldInstallIO{}, true), 0, t.TempDir(), "501", os.Args[0], "-test.run=^TestSuppliedInstallerRunsBelowInteractiveIOPriority$")
	command.Env = append(os.Environ(), "PB_TEST_INSTALL_PRIORITY=1")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("installer must yield CPU and disk to live terminals: %v: %s", err, strings.TrimSpace(string(output)))
	}
}

// A fresh pb install has no live sessions to yield to: its privileged step runs at normal priority (dadape,
// 2026-11-26: idle I/O made a first install take 4 min on a busy disk).
func TestFreshInstallRunsAtNormalPriority(t *testing.T) {
	command := privilegedInstallCommand(t.Context(), 0, t.TempDir(), "501", "/protected/pb", "__runtime-service", "install-supplied")
	for _, arg := range command.Args {
		if arg == "/usr/bin/ionice" || arg == "/usr/bin/nice" {
			t.Fatalf("fresh install runs below normal priority: %v", command.Args)
		}
	}
}
