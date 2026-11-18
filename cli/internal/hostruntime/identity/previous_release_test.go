package identity

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// The identity, registration and network secret 2026.11.16.2 wrote (testdata from that release's Store) load as
// they are after an update: the same key, machine and secret, nothing regenerated.
func TestLoadsIdentityWrittenByPreviousRelease(t *testing.T) {
	root := filepath.Join(t.TempDir(), "identity")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"machine-identity.json", "machine-registration.json", "network-fingerprint-secret.json", "peer-endpoint.json"} {
		raw, err := os.ReadFile(filepath.Join("testdata", "2026.11.16.2-"+name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), raw, 0o600); err != nil {
			t.Fatal(err)
		}
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
	before, _ := os.ReadFile(filepath.Join(root, "peer-endpoint.json"))
	if _, err := store.PeerEndpoint(); err != nil {
		t.Fatalf("previous release's peer endpoint: %v", err)
	}
	if after, _ := os.ReadFile(filepath.Join(root, "peer-endpoint.json")); !bytes.Equal(before, after) {
		t.Fatal("previous release's peer endpoint was regenerated")
	}
}
