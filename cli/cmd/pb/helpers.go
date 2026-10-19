package main

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/internal/command"
	"github.com/spf13/cobra"
)

func actionRun(action command.Action) func(*cobra.Command, []string) error {
	return func(command *cobra.Command, args []string) error {
		c := actionContext(command, args)
		c.Context = context.WithValue(c.Context, confirmationCommandKey{}, command)
		return action(c)
	}
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

const dashboardEnrollmentGuidance = "Run `pb login` and approve this CLI in your browser. The printed link works on another device when this machine has no browser."

var openBrowser = platformOpenBrowser

func relativeTimestamp(at time.Time) string {
	if at.IsZero() {
		return "unknown"
	}
	value := at
	return relativeTime(&value)
}

func newIdempotencyKey() string {
	return "operation_" + uuid.NewString()
}

func relativeTime(at *time.Time) string {
	if at == nil {
		return "-"
	}
	d := time.Since(*at)
	if d < time.Minute {
		return "now"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i, ra := range a {
		current := make([]int, len(b)+1)
		current[0] = i + 1
		for j, rb := range b {
			cost := 0
			if ra != rb {
				cost = 1
			}
			current[j+1] = minInt(current[j]+1, prev[j+1]+1, prev[j]+cost)
		}
		prev = current
	}
	return prev[len(b)]
}

func minInt(values ...int) int {
	value := values[0]
	for _, candidate := range values[1:] {
		if candidate < value {
			value = candidate
		}
	}
	return value
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func orNone(s string) string {
	if s == "" {
		return "(unset)"
	}
	return s
}
