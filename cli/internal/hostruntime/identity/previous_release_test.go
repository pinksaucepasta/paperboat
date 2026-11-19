package identity

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/atomicfile/atomicfiletest"
)

// The identity, registration and network secret 2026.11.16.2 wrote (testdata from that release's Store) load as
// they are after an update: the same key, machine and secret, nothing regenerated.
func TestLoadsIdentityWrittenByPreviousRelease(t *testing.T) {
	root := filepath.Join(t.TempDir(), "identity")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	inbox := t.TempDir()
	for _, name := range []string{"machine-identity.json", "machine-registration.json", "network-fingerprint-secret.json"} {
		raw, err := os.ReadFile(filepath.Join("testdata", "2026.11.16.2-"+name))
		if err != nil {
			t.Fatal(err)
		}
		// The fixture's inbox path is a Unix one; the previous release wrote the machine's own absolute path.
		raw = bytes.ReplaceAll(raw, []byte("/home/fixture/inbox"), []byte(strings.ReplaceAll(filepath.Join(inbox, "inbox"), `\`, `\\`)))
		atomicfiletest.WriteOwnerOnly(t, filepath.Join(root, name), raw)
	}
	store, err := Open(Config{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	registration, err := store.Registration()
	if err != nil || registration.MachineID != "machine_01" || registration.PublicKeyID != store.Current().ID {
		t.Fatalf("previous release's registration = %+v, %v", registration, err)
	}
	raw, _ := os.ReadFile(filepath.Join("testdata", "2026.11.16.2-network-fingerprint-secret.json"))
	want, err := decodeNetworkFingerprintSecret(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := store.NetworkFingerprintSecret(); err != nil || !bytes.Equal(got, want) {
		t.Fatalf("network secret regenerated or refused: %v", err)
	}
}
