package usermachines

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/metering"
)

// lanec-gate showed its pairing version (round 15) as the version it runs after sixteen updates: the
// machine API returned runtime_versions, which is written once at pairing. It reports what the runtime's
// latest observation says, and the installation's own declaration stays what installation matching uses.
func TestMachineReportsTheVersionItsRuntimeLastObserved(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	userID := "usr_runtime_version_" + suffix
	if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, userID, "workos_"+suffix, "runtime-version-"+suffix+"@example.test"); err != nil {
		t.Fatal(err)
	}
	service := New(store, audit.NewWriter(store), Policy{PairingLifetime: 10 * time.Minute, AllowedPlatforms: []string{"linux"}}, testSeatAuthorizer{})
	service.ConfigureInstallationEncryption("runtime-version-key")
	service.ConfigureAccess(nil, "https://api.paperboat.test")
	service.ConfigureHelperEnrollment(func(context.Context, string, string, string, time.Duration) (HelperEnrollmentGrant, error) {
		return HelperEnrollmentGrant{EnrollmentID: "henr_runtime_version_" + suffix, HelperID: "helper_runtime_version_" + suffix, Credential: "runtime-version-credential", ExpiresAt: time.Now().UTC().Add(10 * time.Minute)}, nil
	})
	if err := service.ConfigureRuntimeRoute("runtime.example.test", 38080); err != nil {
		t.Fatal(err)
	}
	paired, err := service.Setup(ctx, userID, SetupInput{Alias: "versioned", Platform: "linux", Architecture: "amd64", WorkspaceRoot: "/home/paperboat", PublicIdentityKey: newIntegrationPublicIdentityKey(t), RuntimeVersions: json.RawMessage(`{"pb":"dev-aaaaaaaa"}`)})
	if err != nil {
		t.Fatal(err)
	}
	pb := func(raw json.RawMessage) string {
		t.Helper()
		var versions map[string]string
		if err := json.Unmarshal(raw, &versions); err != nil {
			t.Fatalf("runtime versions %s: %v", raw, err)
		}
		return versions["pb"]
	}
	// versions returns the machine and the pb version its encoded API response carries.
	versions := func() (UserMachine, string, string) {
		t.Helper()
		machine, err := service.Get(ctx, userID, paired.ID)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(machine)
		if err != nil {
			t.Fatal(err)
		}
		var wire struct {
			RuntimeVersions json.RawMessage `json:"runtime_versions"`
		}
		if err := json.Unmarshal(encoded, &wire); err != nil {
			t.Fatal(err)
		}
		return machine, pb(wire.RuntimeVersions), string(encoded)
	}
	if _, sent, _ := versions(); sent != "dev-aaaaaaaa" {
		t.Fatalf("before any observation the API sends pb %q, want the paired dev-aaaaaaaa", sent)
	}

	// The machine updates to dev-bbbbbbbb; its runtime's heartbeat reports the version it runs.
	observations := metering.NewRuntimeRepository(store, time.Minute)
	if err := observations.RecordRuntimeObservation(ctx, metering.RuntimeObservation{EnvironmentID: paired.EnvironmentID, MachineID: paired.ID, ReporterVersion: "dev-bbbbbbbb"}); err != nil {
		t.Fatal(err)
	}
	machine, sent, encoded := versions()
	if sent != "dev-bbbbbbbb" {
		t.Fatalf("after the update's heartbeat the API sends pb %q, want the running dev-bbbbbbbb", sent)
	}
	if pb(machine.InstalledRuntimeVersions) != "dev-aaaaaaaa" || strings.Contains(encoded, "dev-aaaaaaaa") {
		t.Fatalf("installation declaration = %s, encoded %s: want it kept for installation matching and never sent", machine.InstalledRuntimeVersions, encoded)
	}
}
