package hostruntimecmd

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
)

func TestBootstrapFinalizationRetainsExpiredMaterialWithoutRenewingAuthority(t *testing.T) {
	now, root := time.Now().UTC(), t.TempDir()
	material := testClientBootstrapMaterial("https://control.example.test", now.Add(-time.Minute))
	record := bootstrap.NewResumeRecord(material.ControlURL, "public-key", "token", "Laptop", "client", strings.Repeat("v", 40), now.Add(-time.Minute))
	record.Material, record.PairingStarted = &material, true
	record.ClientInstalled, record.RuntimeEnrolled, record.RuntimeReady = true, true, true
	record.RuntimeListenAddress = "127.0.0.1:12345"
	if err := bootstrap.SaveResume(root, record); err != nil {
		t.Fatal(err)
	}
	reloaded, err := bootstrap.LoadResume(root, record.ServerURL, record.PublicIdentityKey, "", record.DisplayName, record.SetupMode, now)
	if !errors.Is(err, bootstrap.ErrResumeExpired) || !reloaded.RuntimeReady {
		t.Fatalf("finalization checkpoint lost: ready=%t err=%v", reloaded.RuntimeReady, err)
	}
	working := *reloaded.Material
	if err := prepareBootstrapListener(root, &working, &reloaded); err != nil {
		t.Fatalf("local finalization must survive expired pairing material: %v", err)
	}
	working.ReuseIdentity, working.EnrollmentCredential = true, ""
	if reloaded.Material.EnrollmentCredential == "" || reloaded.Material.ReuseIdentity {
		t.Fatal("working recovery mutation changed the protected original binding")
	}
	for _, missing := range []string{"ready", "enrolled", "client", "listener"} {
		invalid := record
		switch missing {
		case "ready":
			invalid.RuntimeReady = false
		case "enrolled":
			invalid.RuntimeEnrolled = false
		case "client":
			invalid.ClientInstalled = false
		case "listener":
			invalid.RuntimeListenAddress = ""
		}
		if err := bootstrap.SaveResume(root, invalid); err == nil {
			t.Fatalf("expired material accepted without %s checkpoint", missing)
		}
	}
}

func TestBootstrapFinalizationRequiresExactCurrentEnrollment(t *testing.T) {
	material := testClientBootstrapMaterial("https://control.example.test", time.Now().Add(-time.Minute))
	record := bootstrap.ResumeRecord{RuntimeReady: true, RuntimeEnrolled: true, ClientInstalled: true, Material: &material, ServerURL: material.ControlURL, PublicIdentityKey: "public-key"}
	registration := identity.Registration{AccountID: "account-a", ServerURL: record.ServerURL, PublicIdentityKey: record.PublicIdentityKey, MachineID: material.UserMachineID, EnvironmentID: material.EnvironmentID, InstallationGeneration: material.InstallationGeneration, SetupMode: material.SetupMode}
	if err := validateBootstrapFinalizationBinding(registration, "account-a", record); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"account", "server", "key", "machine", "environment", "generation", "mode"} {
		t.Run(field, func(t *testing.T) {
			changed := registration
			switch field {
			case "account":
				changed.AccountID = "account-b"
			case "server":
				changed.ServerURL = "https://other.example.test"
			case "key":
				changed.PublicIdentityKey = "other"
			case "machine":
				changed.MachineID = "other"
			case "environment":
				changed.EnvironmentID = "other"
			case "generation":
				changed.InstallationGeneration++
			case "mode":
				changed.SetupMode = "host"
			}
			if err := validateBootstrapFinalizationBinding(changed, "account-a", record); !errors.Is(err, bootstrap.ErrResumeBinding) {
				t.Fatalf("changed enrollment accepted: %v", err)
			}
		})
	}
}
