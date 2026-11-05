//go:build darwin || linux

package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	doctorpkg "github.com/pinksaucepasta/paperboat/internal/doctor"
	"github.com/pinksaucepasta/paperboat/internal/localdaemon"
)

// daemonGenerationLines describes each running daemon generation: the live one, and every one still
// draining the connections (forwards, execs, terminals, SSH) it held at an upgrade, with their count and
// its drain deadline.
func daemonGenerationLines(now time.Time) ([]string, error) {
	paths, err := currentLocalDaemonPaths()
	if err != nil {
		return nil, err
	}
	states, err := localdaemon.GenerationStates(paths.StateRoot, now)
	if err != nil {
		return nil, err
	}
	lines := make([]string, 0, len(states))
	for _, state := range states {
		line := fmt.Sprintf("%s %s: %d held connection", state.State, state.Generation[:8], state.Streams)
		if state.Streams != 1 {
			line += "s"
		}
		if state.State == "draining" {
			line += ", ends by " + state.DrainUntil.Local().Format(time.RFC3339)
		}
		lines = append(lines, line)
	}
	return lines, nil
}

func writeDaemonGenerations(output io.Writer) {
	lines, err := daemonGenerationLines(time.Now())
	if err != nil || len(lines) == 0 {
		return
	}
	fmt.Fprintf(output, "Daemon generations: %s\n", strings.Join(lines, "; "))
}

func doctorDaemonGenerationProbes() []doctorpkg.Probe {
	return []doctorpkg.Probe{{Code: "daemon_generations", Run: func(context.Context) doctorpkg.Check {
		check := doctorpkg.Check{Category: "local", Code: "daemon_generations", Status: doctorpkg.StatusPass}
		lines, err := daemonGenerationLines(time.Now())
		switch {
		case err != nil:
			check.Status, check.Summary, check.Recovery = doctorpkg.StatusUnavailable, "The running daemon generations could not be read.", "Run pb doctor again."
		case len(lines) == 0:
			check.Summary = "No daemon generation reports itself running."
		default:
			check.Summary = "Daemon generations: " + strings.Join(lines, "; ") + "."
		}
		return check
	}}}
}
