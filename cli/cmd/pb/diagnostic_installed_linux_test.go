//go:build linux

package main

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestInstalledConfigQueryWithoutDesktopEnvironment(t *testing.T) {
	if os.Getenv("PAPERBOAT_TEST_INSTALLED_DIAGNOSTICS") != "1" {
		t.Skip("requires native user service manager")
	}
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	// Query the actual user manager without changing or creating a unit.
	query := exec.CommandContext(ctx, "systemctl", "--user", "show", "--property=Version")
	prepareConfigServiceQuery(query)
	if err := query.Run(); err != nil {
		t.Fatal("current user's native service manager could not be queried without desktop environment")
	}
	if os.Getenv("XDG_RUNTIME_DIR") != "" || os.Getenv("DBUS_SESSION_BUS_ADDRESS") != "" {
		t.Fatal("diagnostic query changed the parent process environment")
	}
}
