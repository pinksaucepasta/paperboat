//go:build windows

package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/connector"
)

func TestWindowsProductionHostRejectsIncompleteConfiguration(t *testing.T) {
	_, err := NewProductionHost(context.Background(), "test", func(string) string { return "" })
	if !errors.Is(err, ErrProductionInvalid) {
		t.Fatalf("error = %v, want production configuration rejection", err)
	}
}

func TestWindowsWorkspaceRejectsRelativePath(t *testing.T) {
	_, err := windowsWorkspace(func(key string) string {
		if key == "PAPERBOAT_WORKSPACE_ROOT" {
			return "relative"
		}
		return ""
	})
	if err == nil {
		t.Fatal("relative Windows workspace was accepted")
	}
}

func TestWindowsConnectorServiceReportsDisconnected(t *testing.T) {
	service := &windowsConnectorService{status: func() connector.Status { return connector.Status{} }}
	status := service.Status()
	if status.Connected || status.Stopping || status.Generation != 0 {
		t.Fatalf("status = %#v", status)
	}
}

func TestWindowsRuntimeDurationUsesSafeFallback(t *testing.T) {
	if got := durationRuntime("not-a-duration", time.Second); got != time.Second {
		t.Fatalf("duration = %s", got)
	}
}
