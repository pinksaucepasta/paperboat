//go:build linux

package machineguard

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func TestLinuxHistoricalDNSCleanupRequiresOwnedLinkAndRevertsFirst(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(root, "paperboat-dns")
	if err := os.Mkdir(link, 0700); err != nil {
		t.Fatal(err)
	}
	aliasPath := filepath.Join(link, "ifalias")
	if err := os.WriteFile(aliasPath, []byte("paperboat-machineguard-v1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var calls []string
	run := func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil, nil
	}
	if err := cleanupLinuxLegacyDNSLink(t.Context(), link, run); err != nil {
		t.Fatal(err)
	}
	want := []string{"resolvectl revert paperboat-dns", "ip link delete paperboat-dns"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("cleanup commands = %v, want %v", calls, want)
	}

	calls = nil
	if err := os.WriteFile(aliasPath, []byte("administrator-owned\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := cleanupLinuxLegacyDNSLink(t.Context(), link, run); err == nil {
		t.Fatal("foreign interface accepted")
	}
	if len(calls) != 0 {
		t.Fatalf("foreign interface triggered cleanup commands: %v", calls)
	}
	if err := os.Remove(aliasPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(aliasPath, 0700); err != nil {
		t.Fatal(err)
	}
	calls = nil
	err := cleanupLinuxLegacyDNSLink(t.Context(), link, run)
	if !errors.Is(err, syscall.EISDIR) || strings.Contains(err.Error(), aliasPath) {
		t.Fatalf("ownership read failure did not preserve a private-safe cause: %v", err)
	}
	if len(calls) != 0 {
		t.Fatalf("ownership read failure triggered cleanup commands: %v", calls)
	}
	if err := os.Remove(aliasPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(aliasPath, []byte("paperboat-machineguard-v1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, failedCommand := range []string{"resolvectl", "ip"} {
		calls = nil
		cause := syscall.EIO
		err := cleanupLinuxLegacyDNSLink(t.Context(), link, func(_ context.Context, name string, args ...string) ([]byte, error) {
			calls = append(calls, name+" "+strings.Join(args, " "))
			if name == failedCommand {
				return []byte("private command output"), cause
			}
			return nil, nil
		})
		if !errors.Is(err, cause) || strings.Contains(err.Error(), "private command output") || strings.Contains(err.Error(), cause.Error()) {
			t.Fatalf("%s failure lost its safe original cause: %v", failedCommand, err)
		}
		wantCalls := 1
		if failedCommand == "ip" {
			wantCalls = 2
		}
		if len(calls) != wantCalls {
			t.Fatalf("%s failure calls = %v, want %d ordered calls", failedCommand, calls, wantCalls)
		}
	}

	if err := cleanupLinuxLegacyDNSLink(t.Context(), filepath.Join(root, "absent"), func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("command should not run for an absent interface")
	}); err != nil {
		t.Fatal(err)
	}
}
