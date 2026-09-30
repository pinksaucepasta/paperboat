package deviceguard

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDeviceHostsPreservesOutsideAndUsesOneNameForAllPorts(t *testing.T) {
	for _, original := range [][]byte{[]byte("  # untouched\r\n127.0.0.1 localhost # existing\r\n"), []byte("127.0.0.1 localhost"), []byte("\xef\xbb\xbf# UTF-8\r\n")} {
		names := map[string]string{"hp.pprbt": "127.100.0.4", "victus.pprbt": "127.100.0.2"}
		next, err := renderDeviceHosts(original, names)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Count(next, []byte(" hp.pprbt")) != 1 || bytes.Contains(next, []byte("3000.")) {
			t.Fatalf("unexpected mappings %q", next)
		}
		again, err := renderDeviceHosts(next, names)
		if err != nil || !bytes.Equal(again, next) {
			t.Fatalf("unstable render %v", err)
		}
		removed, err := renderDeviceHosts(next, nil)
		if err != nil || !bytes.Equal(removed, original) {
			t.Fatalf("outside bytes changed %q → %q: %v", original, removed, err)
		}
	}
}
func TestDeviceHostsRejectsCorruptionAndExternalConflicts(t *testing.T) {
	names := map[string]string{"hp.pprbt": "127.100.0.4"}
	for _, original := range []string{hostsStart + "\n", hostsEnd + "\n", hostsStart + "\n" + hostsStart + "\n" + hostsEnd + "\n", hostsStart + "\n1.2.3.4 unrelated.com\n" + hostsEnd + "\n", "127.0.0.1 HP.PPRBT # admin\n", " " + hostsStart + "\n"} {
		if _, err := renderDeviceHosts([]byte(original), names); err == nil {
			t.Fatalf("corruption accepted %q", original)
		}
	}
	for name, address := range map[string]string{"3000.hp.pprbt": "127.100.0.4", "hp.pprbt": "192.0.2.1", "gateway.local.pprbt.dev": "127.100.0.1"} {
		if _, err := renderDeviceHosts(nil, map[string]string{name: address}); err == nil {
			t.Fatal("unsafe projection accepted")
		}
	}
}
func TestDeviceHostsActualFileReplacementAndCleanup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	original := []byte("# foreign content\r\n127.0.0.1 localhost\r\n")
	if err := os.WriteFile(path, original, 0640); err != nil {
		t.Fatal(err)
	}
	if err := updateDeviceHosts(t.Context(), path, map[string]string{"hp.pprbt": "127.100.0.4"}); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := updateDeviceHosts(t.Context(), path, map[string]string{"hp.pprbt": "127.100.0.4"}); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if !os.SameFile(before, after) {
		t.Fatal("unchanged mapping rewrote file")
	}
	if err := updateDeviceHosts(t.Context(), path, nil); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !bytes.Equal(data, original) {
		t.Fatal("cleanup changed unrelated data")
	}
	before, _ = os.Stat(path)
	edited := append([]byte(nil), original...)
	edited = append(edited, []byte("# concurrent writer\n")...)
	if err := os.WriteFile(path, edited, 0640); err != nil {
		t.Fatal(err)
	}
	if err := replaceHostsFile(context.Background(), path, before, original, []byte("replacement")); err == nil {
		t.Fatal("concurrent modification overwritten")
	}
	data, _ = os.ReadFile(path)
	if !bytes.Equal(data, edited) {
		t.Fatal("concurrent content lost")
	}
	if err := os.WriteFile(path, []byte(hostsStart+"\n"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := updateDeviceHosts(t.Context(), path, nil); err == nil {
		t.Fatal("corrupt file replaced")
	}
	data, _ = os.ReadFile(path)
	if string(data) != hostsStart+"\n" {
		t.Fatal("corrupt file changed")
	}
}

func testHostsFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hosts")
	if err := os.WriteFile(path, []byte("127.0.0.1 localhost\n"), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}
