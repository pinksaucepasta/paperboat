package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// P0 footgun: `pb approve <id> --revoke` used to disconnect the machine on the
// server with no confirmation. It must now refuse before touching the daemon,
// and nothing in the daemon may call the server disconnect.
func TestApproveRevokeRefusesAndPointsToMachineRevoke(t *testing.T) {
	var out bytes.Buffer
	if code := run(t.Context(), []string{"approve", "machine_other", "--revoke"}, &out, &out); code == 0 || !strings.Contains(out.String(), "pb machine revoke") || !strings.Contains(out.String(), "confirmation") {
		t.Fatalf("exit=%d output=%s", code, out.String())
	}
}

func TestOnlyMachineRevokeAndTheDesktopActionCallTheServerDisconnect(t *testing.T) {
	callers := map[string]bool{}
	for _, root := range []string{filepath.Join("..", "..", "internal"), "."} {
		_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			data, readErr := os.ReadFile(path)
			if readErr == nil && strings.Contains(string(data), ".DisconnectUserMachine(") {
				callers[filepath.Base(path)] = true
			}
			return nil
		})
	}
	for name := range callers {
		if name != "machine_commands.go" && name != "desktop_command.go" {
			t.Fatalf("%s calls the server disconnect; revoking a machine is pb machine revoke with its confirmation", name)
		}
	}
	if !callers["machine_commands.go"] {
		t.Fatal("pb machine revoke no longer reaches the server disconnect")
	}
}
