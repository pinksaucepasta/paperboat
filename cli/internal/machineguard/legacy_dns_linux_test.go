//go:build linux

package machineguard

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

	if err := cleanupLinuxLegacyDNSLink(t.Context(), filepath.Join(root, "absent"), func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("command should not run for an absent interface")
	}); err != nil {
		t.Fatal(err)
	}
}
