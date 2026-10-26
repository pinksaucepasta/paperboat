//go:build linux

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	doctorpkg "github.com/pinksaucepasta/paperboat/internal/doctor"
)

// doctorCarrierProbes reports a native carrier that keeps failing to start. The carrier holds every private
// connection to this computer, so while it restarts in a loop every ping and exec to it fails, and the
// local daemon still reports itself ready.
func doctorCarrierProbes() []doctorpkg.Probe {
	unit := "paperboat-carrier-u" + strconv.Itoa(os.Getuid()) + ".service"
	return []doctorpkg.Probe{{Code: "native_carrier", Run: func(ctx context.Context) doctorpkg.Check {
		return doctorCarrierCheck(ctx, doctorSystemdRuntime, unit, systemdUnitProperties)
	}}}
}

// systemdUnitProperties asks systemd about a system unit; it needs no privilege.
func systemdUnitProperties(ctx context.Context, unit string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "systemctl", "show", unit, "--property=LoadState", "--property=ActiveState", "--property=SubState", "--property=Result", "--property=NRestarts").Output()
	if err != nil {
		return nil, err
	}
	properties := map[string]string{}
	for _, line := range strings.Split(string(output), "\n") {
		if key, value, ok := strings.Cut(line, "="); ok {
			properties[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	return properties, nil
}

func doctorCarrierCheck(ctx context.Context, systemdRuntime, unit string, properties func(context.Context, string) (map[string]string, error)) doctorpkg.Check {
	check := doctorpkg.Check{Category: "local", Code: "native_carrier"}
	if _, err := os.Stat(systemdRuntime); err != nil {
		check.Status, check.Summary = doctorpkg.StatusPass, "This host does not run the native carrier under systemd."
		return check
	}
	values, err := properties(ctx, unit)
	if err != nil {
		check.Status, check.Summary = doctorpkg.StatusUnavailable, "Whether the native carrier is running could not be checked."
		check.Recovery = "Run pb doctor again."
		return check
	}
	switch {
	case values["LoadState"] == "not-found":
		check.Status, check.Summary = doctorpkg.StatusPass, "This computer runs no separate native carrier."
	case values["ActiveState"] == "active":
		check.Status, check.Summary = doctorpkg.StatusPass, "The native carrier is running."
	case values["ActiveState"] == "activating" && values["SubState"] == "start" && values["Result"] == "success":
		// An ordinary start, as right after an update; a failing carrier shows its failed result between tries.
		check.Status, check.Summary = doctorpkg.StatusPending, "The native carrier is starting."
		check.Recovery = "Run `pb doctor` again in a moment."
	default:
		check.Status = doctorpkg.StatusWarning
		check.Summary = "The native carrier keeps failing to start, so private connections to this computer do not work"
		if restarts, err := strconv.Atoi(values["NRestarts"]); err == nil && restarts > 0 {
			check.Summary += fmt.Sprintf(" (restarted %d times)", restarts)
		}
		check.Summary += "."
		check.Recovery = "See why with `journalctl -u " + unit + "`. If you installed an older Paperboat over a newer one, run `pb update` to return to the newer version, or run `pb uninstall` and install the older one again."
	}
	return check
}
