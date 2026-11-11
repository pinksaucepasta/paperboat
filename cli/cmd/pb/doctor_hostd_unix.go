//go:build darwin || linux

package main

import (
	"context"
	"errors"
	"net"
	"os"
	"runtime"
	"time"

	doctorpkg "github.com/pinksaucepasta/paperboat/internal/doctor"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
)

// doctorHostdProbes checks that the native host supervisor answers. Exec, files, terminals and the machine's
// online state all go through it, but the local daemon reports itself ready without it (pbvm1 F14-03: a failed
// update's rollback left no hostd, the machine was offline, and doctor said healthy). A stale socket file from a
// stopped hostd survives (RuntimeDirectoryPreserve), so the check connects instead of looking for the file.
func doctorHostdProbes() []doctorpkg.Probe {
	return []doctorpkg.Probe{{Code: "hostd", Run: func(ctx context.Context) doctorpkg.Check {
		layout, err := service.UserLayout(runtime.GOOS, os.Getuid())
		if err != nil {
			return doctorpkg.Check{Category: "local", Code: "hostd", Status: doctorpkg.StatusUnavailable, Summary: "Whether the host service answers could not be checked.", Recovery: "Run pb doctor again."}
		}
		return doctorHostdCheck(ctx, layout.Binary, layout.HostdSocket, dialHostdSocket, doctorHostdWindow, doctorHostdInterval)
	}}}
}

func dialHostdSocket(ctx context.Context, socket string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return err
	}
	return connection.Close()
}

// A hostd restart or hand-over (an update starts the next generation before the serving one exits) refuses
// connections for a moment; doctor retries for this long (inside the doctor probe budget) before it calls the service not running. A refusal
// cannot tell a restart from a stopped service (a stopped one leaves its socket file too), so the wording after
// the window stays "not running" and says to run doctor again if an update just finished.
const (
	doctorHostdWindow   = 6 * time.Second // the probe budget is 10 s and one dial takes up to 3 s
	doctorHostdInterval = 500 * time.Millisecond
)

// doctorHostdCheck: a computer without the native installation (no executable slot) has no host service to check.
func doctorHostdCheck(ctx context.Context, installed, socket string, dial func(context.Context, string) error, window, interval time.Duration) doctorpkg.Check {
	check := doctorpkg.Check{Category: "local", Code: "hostd"}
	if _, err := os.Stat(installed); errors.Is(err, os.ErrNotExist) {
		check.Status, check.Summary = doctorpkg.StatusPass, "This computer runs no Paperboat host service."
		return check
	}
	err := dial(ctx, socket)
	for deadline := time.Now().Add(window); err != nil && time.Now().Before(deadline) && ctx.Err() == nil; {
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
		if ctx.Err() != nil {
			break
		}
		err = dial(ctx, socket)
	}
	if err != nil {
		check.Status, check.Summary = doctorpkg.StatusFail, "The Paperboat host service is not running, so this computer is offline for exec, files and terminals."
		check.Recovery = "If an update just finished, run `pb doctor` again in a moment; otherwise run `pb install` to repair it."
		return check
	}
	check.Status, check.Summary = doctorpkg.StatusPass, "The Paperboat host service answers."
	return check
}
