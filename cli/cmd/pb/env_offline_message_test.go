package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

// Offline, a computer without an ENV vault cannot tell "never set up" from "this account uses ENV": it says ENV needs Paperboat online.
func TestEnvWithoutAVaultWhileOfflineSaysItNeedsPaperboatOnline(t *testing.T) {
	original := readDaemonSnapshot
	defer func() { readDaemonSnapshot = original }()
	readDaemonSnapshot = func(context.Context) (localapi.Snapshot, error) {
		return localapi.Snapshot{Health: []localapi.HealthItem{{Code: "control_plane_unavailable"}}}, nil
	}
	if got := safeEnvironmentVariableCommandError(config.ErrSecretNotFound); got == nil || !strings.Contains(got.Error(), "ENV needs Paperboat online") || strings.Contains(got.Error(), "not set up") {
		t.Fatalf("offline message=%v", got)
	}
	readDaemonSnapshot = func(context.Context) (localapi.Snapshot, error) { return localapi.Snapshot{}, nil }
	if got := safeEnvironmentVariableCommandError(config.ErrSecretNotFound); got == nil || !strings.Contains(got.Error(), "not set up") || strings.Contains(got.Error(), "needs Paperboat online") {
		t.Fatalf("online message=%v", got)
	}
	readDaemonSnapshot = func(context.Context) (localapi.Snapshot, error) {
		return localapi.Snapshot{}, errors.New("daemon down")
	}
	if got := safeEnvironmentVariableCommandError(config.ErrSecretNotFound); got == nil || !strings.Contains(got.Error(), "not set up") {
		t.Fatalf("unknown daemon message=%v", got)
	}
}

func TestPendingEnvironmentLaunchAlsoSaysEnvNeedsPaperboatOnline(t *testing.T) {
	if line := launchWarningLine([]string{"environment_pending"}); !strings.Contains(line, "ENV needs Paperboat online") || !strings.Contains(line, "pb env vault unlock") {
		t.Fatalf("pending line=%q", line)
	}
}
