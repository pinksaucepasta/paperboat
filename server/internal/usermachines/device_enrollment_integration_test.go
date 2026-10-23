package usermachines

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
)

// A device login approved for a team mints an enrollment that, when the device pairs, puts the
// machine into that team in the same transaction; Personal enrollments place nothing.
func TestDeviceEnrollmentPlacesMachineInChosenWorkspace(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	user, outsider := "usr_devenroll_"+suffix, "usr_devenroll_out_"+suffix
	team, otherTeam := "devenroll-team-"+suffix, "devenroll-other-"+suffix
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{user, outsider} {
		exec(`INSERT INTO paperboat.users (id, workos_subject, primary_email, status) VALUES ($1,$2,$3,'active')`, id, "workos_"+id, id+"@example.test")
	}
	exec(`INSERT INTO paperboat.user_machine_entitlements (id,user_id,provider_subscription_id,product_code,state,seat_quantity,allowance_bytes,current_period_start,current_period_end) VALUES ($1,$2,$3,'connected-test','active',3,1048576,now()-interval '1 hour',now()+interval '1 hour')`, "ume_devenroll_"+suffix, user, "sub_devenroll_"+suffix)
	exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1),($3,$4,1)`, team, user, otherTeam, outsider)
	exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true),($3,$4,1,'member',true)`, team, user, otherTeam, outsider)

	service := New(store, audit.NewWriter(store), Policy{PairingLifetime: 10 * time.Minute, AllowedPlatforms: []string{"linux"}}, testSeatAuthorizer{})
	service.ConfigureInstallationEncryption("test-enrollment-key")
	service.ConfigureAccess(nil, "https://api.paperboat.test")
	service.ConfigureBootstrapCommand("pb machine bootstrap")
	if err := service.ConfigureRuntimeRoute("runtime.example.test", 38080); err != nil {
		t.Fatal(err)
	}
	service.ConfigureHelperEnrollment(func(context.Context, string, string, string, time.Duration) (HelperEnrollmentGrant, error) {
		return HelperEnrollmentGrant{EnrollmentID: "henr_devenroll_" + suffix, HelperID: "hlp_devenroll_" + suffix, Credential: "enrollment-credential", ExpiresAt: time.Now().UTC().Add(10 * time.Minute)}, nil
	})

	if _, err := service.IssueDeviceEnrollment(ctx, user, "grant-out-"+suffix, otherTeam, "posix"); !errors.Is(err, ErrWorkspaceForbidden) {
		t.Fatalf("enrollment for a team the account is not in: %v", err)
	}

	first, err := service.IssueDeviceEnrollment(ctx, user, "grant-team-"+suffix, team, "posix")
	if err != nil {
		t.Fatal(err)
	}
	again, err := service.IssueDeviceEnrollment(ctx, user, "grant-team-"+suffix, team, "posix")
	if err != nil || again.BootstrapToken != first.BootstrapToken {
		t.Fatalf("a retried poll of the same grant must return the same enrollment: %v", err)
	}
	if _, err := service.CreatePairing(ctx, PairingInput{EnrollmentToken: first.BootstrapToken, Verifier: "verifier-team-" + suffix, Alias: "teambox", Platform: "linux", Architecture: "amd64", WorkspaceRoot: "/home/dev", RuntimeVersions: json.RawMessage(`{"pb":"test"}`), PublicIdentityKey: newIntegrationPublicIdentityKey(t)}); err != nil {
		t.Fatal(err)
	}
	var bound int
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.team_resource_bindings b JOIN paperboat.user_machines m ON m.id=b.resource_id WHERE b.team_id=$1 AND b.resource_kind='machine' AND b.active AND m.user_id=$2 AND m.alias='teambox'`, team, user).Scan(&bound); err != nil || bound != 1 {
		t.Fatalf("machine was not placed in the chosen team: bound=%d err=%v", bound, err)
	}

	personal, err := service.IssueDeviceEnrollment(ctx, user, "grant-personal-"+suffix, "", "posix")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreatePairing(ctx, PairingInput{EnrollmentToken: personal.BootstrapToken, Verifier: "verifier-personal-" + suffix, Alias: "homebox", Platform: "linux", Architecture: "amd64", WorkspaceRoot: "/home/dev2", RuntimeVersions: json.RawMessage(`{"pb":"test"}`), PublicIdentityKey: newIntegrationPublicIdentityKey(t)}); err != nil {
		t.Fatal(err)
	}
	if err := store.SQL().QueryRowContext(ctx, `SELECT count(*) FROM paperboat.team_resource_bindings b JOIN paperboat.user_machines m ON m.id=b.resource_id WHERE b.resource_kind='machine' AND m.alias='homebox' AND m.user_id=$1`, user).Scan(&bound); err != nil || bound != 0 {
		t.Fatalf("a Personal enrollment must not join a team: bound=%d err=%v", bound, err)
	}
}
