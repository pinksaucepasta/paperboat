//go:build windows

package hostinstall

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
)

func TestWindowsPurgeRemovesUnregisteredOwnedDeclaration(t *testing.T) {
	allowed, err := checkAdministrator()
	if err != nil || !allowed {
		t.Skip("requires elevated native Windows cleanup fixture")
	}
	ownerSID := fmt.Sprintf("S-1-5-21-49157-49158-49159-%d", uint32(time.Now().UnixNano()))
	layout, err := service.WindowsUserLayout(ownerSID)
	if err != nil {
		t.Fatal(err)
	}
	root, err := WindowsInstanceRoot(layout.Instance)
	if err != nil {
		t.Fatal(err)
	}
	name := "PaperboatLocalDaemon-" + layout.Instance
	path := filepath.Join(`C:\ProgramData\Paperboat\services`, name+".json")
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("isolated declaration already exists or cannot be inspected")
	}
	body, err := json.Marshal(map[string]any{
		"schema": "paperboat.windows-service/v1", "name": name,
		"display_name": name, "description": "test-owned stopped declaration",
		"executable": layout.Binary, "arguments": []string{"daemon", "__runtime-local-daemon", "--instance", layout.Instance},
		"account": "Paperboat",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })
	generation := strings.Repeat("a", 32)
	generationName, err := service.WindowsSessionOwnerName(layout.Instance, generation)
	if err != nil {
		t.Fatal(err)
	}
	generationPath := filepath.Join(filepath.Dir(path), generationName+".json")
	generationBody, err := json.Marshal(map[string]any{
		"schema": "paperboat.windows-service/v1", "name": generationName,
		"display_name": generationName, "description": "test-owned stopped generation",
		"executable": layout.Binary, "arguments": []string{"daemon", "__runtime-session-owner", "--generation", generation, "--instance", layout.Instance},
		"account": "Paperboat",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(generationPath, generationBody, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(generationPath) })
	ctx := context.Background()
	t.Run("inventory", func(t *testing.T) {
		items, err := accountLeftoversKeepingCLI(ctx, layout, root, layout.Binary)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(items, ","), "service declaration") {
			t.Fatalf("unregistered owned declaration omitted from cleanup inventory: %v", items)
		}
	})
	t.Run("cleanup", func(t *testing.T) {
		if err := uninstallWindowsKeepingCLI(ctx, layout, true, ownerSID, layout.Binary); err != nil {
			t.Fatal(err)
		}
		for _, declaration := range []string{path, generationPath} {
			if _, err := os.Lstat(declaration); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("successful purge retained owned declaration: %v", err)
			}
		}
	})
	t.Run("malformed-declaration", func(t *testing.T) {
		malformed := []byte("{}")
		if err := os.WriteFile(path, malformed, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := uninstallWindowsKeepingCLI(ctx, layout, true, ownerSID, layout.Binary); err == nil {
			t.Fatal("malformed owned declaration was accepted for deletion")
		}
		retained, err := os.ReadFile(path)
		if err != nil || string(retained) != string(malformed) {
			t.Fatalf("refused declaration must remain unchanged: error=%v", err)
		}
	})
}
