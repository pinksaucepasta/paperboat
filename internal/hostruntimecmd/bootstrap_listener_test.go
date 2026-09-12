package hostruntimecmd

import (
	"encoding/base64"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
)

func TestFreshBootstrapCannotReplaceExistingEnrollment(t *testing.T) {
	root := t.TempDir()
	store, err := identity.Open(identity.Config{StateRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	key := store.Current()
	if err := store.SaveRegistration(identity.Registration{ServerURL: "https://api.example.test", MachineID: "machine_1", EnvironmentID: "env_1", PublicKeyID: key.ID, PublicIdentityKey: base64.RawURLEncoding.EncodeToString(key.Public()), InboxPath: filepath.Join(root, "inbox"), InstallationGeneration: 1, SetupRoles: []string{"interactive"}, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := rejectFreshBootstrapOverEnrollment(store, bootstrap.ErrResumeNotFound); err == nil || !strings.Contains(err.Error(), "pb uninstall") {
		t.Fatalf("guard error=%v", err)
	}
	if err := rejectFreshBootstrapOverEnrollment(store, nil); err != nil {
		t.Fatalf("resume was blocked: %v", err)
	}
}

func TestAllocatedBootstrapListenerIsConcreteLoopback(t *testing.T) {
	address, err := allocateBootstrapLoopbackAddress()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(address, "127.0.0.1:") || strings.HasSuffix(address, ":0") {
		t.Fatalf("address=%q", address)
	}
}

func TestBootstrapListenerSurvivesCredentialConsumptionAndMaterialRecovery(t *testing.T) {
	root, now := t.TempDir(), time.Now().UTC()
	material := testClientBootstrapMaterial("https://control.example.test", now.Add(time.Hour))
	publicKey := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	record := bootstrap.NewResumeRecord(material.ControlURL, publicKey, "token", "Laptop", "client", strings.Repeat("v", 40), now.Add(time.Hour))
	record.PairingStarted, record.Material = true, &material
	if err := prepareBootstrapListener(root, &material, &record); err != nil {
		t.Fatal(err)
	}
	address := material.HelperListenAddress
	material.EnrollmentCredential = ""
	record.RuntimeEnrolled = true
	if err := bootstrap.SaveResume(root, record); err != nil {
		t.Fatalf("checkpoint after credential consumption: %v", err)
	}
	reloaded, err := bootstrap.LoadResume(root, record.ServerURL, publicKey, "", "Laptop", "client", now)
	if err != nil {
		t.Fatal(err)
	}
	// Fresh server material contains its default listener; the local journal
	// remains the authority for the already selected per-user address.
	recovered := testClientBootstrapMaterial(record.ServerURL, now.Add(time.Hour))
	reloaded.Material = &recovered
	if err := prepareBootstrapListener(root, &recovered, &reloaded); err != nil {
		t.Fatal(err)
	}
	if recovered.HelperListenAddress != address || reloaded.RuntimeListenAddress != address {
		t.Fatal("material recovery replaced the selected listener")
	}
	for _, invalid := range []string{"0.0.0.0:1234", "127.0.0.1:0", "127.0.0.1:http"} {
		reloaded.RuntimeListenAddress = invalid
		if err := bootstrap.SaveResume(root, reloaded); err == nil {
			t.Fatalf("accepted invalid listener %q", invalid)
		}
	}
}
