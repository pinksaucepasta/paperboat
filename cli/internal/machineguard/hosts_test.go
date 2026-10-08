package machineguard

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestMachineHostsCleanupPreservesOutsideAndRemovesOnlyOwnedBlock(t *testing.T) {
	for _, original := range [][]byte{
		[]byte("  # untouched\r\n127.0.0.1 localhost # existing\r\n"),
		[]byte("127.0.0.1 localhost"),
		[]byte("\xef\xbb\xbf# UTF-8\r\n"),
	} {
		owned := append([]byte(hostsStart+"\n127.100.0.4 hp.pprbt\n127.100.0.5 hp.local.pprbt.dev\n"+hostsEnd+"\n"), original...)
		cleaned, err := renderMachineHosts(owned)
		if err != nil || !bytes.Equal(cleaned, original) {
			t.Fatalf("owned block cleanup %q → %q: %v", owned, cleaned, err)
		}
	}
}

func TestMachineHostsCleanupRejectsCorruptionWithoutChangingInput(t *testing.T) {
	for _, original := range []string{
		hostsStart + "\n",
		hostsEnd + "\n",
		hostsStart + "\n" + hostsStart + "\n" + hostsEnd + "\n",
		hostsStart + "\n1.2.3.4 unrelated.com\n" + hostsEnd + "\n",
		hostsStart + "\n127.0.0.1 malformed..pprbt\n" + hostsEnd + "\n",
		" " + hostsStart + "\n",
	} {
		if _, err := renderMachineHosts([]byte(original)); err == nil {
			t.Fatalf("corruption accepted %q", original)
		}
	}
	if _, err := renderMachineHosts([]byte("127.0.0.1 HP.PPRBT # admin\n")); err != nil {
		t.Fatalf("unmarked administrator entry was changed or rejected: %v", err)
	}
}

func TestMachineHostsActualFileCleanupAndConcurrentWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts")
	original := []byte("# foreign content\r\n127.0.0.1 localhost\r\n")
	owned := append([]byte(hostsStart+"\r\n127.100.0.4 hp.pprbt\r\n"+hostsEnd+"\r\n"), original...)
	if err := os.WriteFile(path, owned, 0640); err != nil {
		t.Fatal(err)
	}
	if err := removeOwnedMachineHosts(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, original) {
		t.Fatalf("cleanup changed unrelated data %q %v", data, err)
	}
	before, _ := os.Stat(path)
	edited := append(bytes.Clone(original), []byte("# concurrent writer\n")...)
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
	corrupt := []byte(hostsStart + "\n")
	if err := os.WriteFile(path, corrupt, 0640); err != nil {
		t.Fatal(err)
	}
	if err := removeOwnedMachineHosts(t.Context(), path); err == nil {
		t.Fatal("corrupt file replaced")
	}
	data, _ = os.ReadFile(path)
	if !bytes.Equal(data, corrupt) {
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
