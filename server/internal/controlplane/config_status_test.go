package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
)

func TestNewlyPublishedRevisionRejectsHeartbeatsAndEmptyReports(t *testing.T) {
	tests := []struct {
		previous sql.NullString
		reported string
		want     string
		changed  bool
	}{
		{reported: "revision-1", want: "revision-1", changed: true},
		{previous: sql.NullString{String: "revision-1", Valid: true}, reported: "revision-1", want: "revision-1"},
		{previous: sql.NullString{String: "revision-1", Valid: true}, reported: " revision-2 ", want: "revision-2", changed: true},
		{previous: sql.NullString{String: "revision-1", Valid: true}, reported: ""},
	}
	for _, test := range tests {
		got, changed := newlyPublishedRevision(test.previous, test.reported)
		if got != test.want || changed != test.changed {
			t.Fatalf("newlyPublishedRevision(%+v, %q) = %q, %v; want %q, %v", test.previous, test.reported, got, changed, test.want, test.changed)
		}
	}
}

func TestMarshalConfigStatusListCanonicalizesNilAsArray(t *testing.T) {
	for _, encode := range []func() ([]byte, error){
		func() ([]byte, error) { return marshalConfigStatusList([]ConfigStatusPath(nil)) },
		func() ([]byte, error) { return marshalConfigStatusList([]string(nil)) },
	} {
		encoded, err := encode()
		if err != nil {
			t.Fatal(err)
		}
		var value any
		if err = json.Unmarshal(encoded, &value); err != nil {
			t.Fatal(err)
		}
		if _, ok := value.([]any); !ok {
			t.Fatalf("encoded value = %s, want JSON array", encoded)
		}
	}
}

func TestConfigStatusValidationRejectsStaleLeakingAndInconsistentReports(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	valid := ConfigStatusReport{
		State: "healthy", Mode: ConfigModeBidirectional, RepositoryID: "repository", AssignmentID: "assignment",
		EnvironmentID: "environment", MachineID: "helper", InstallationGeneration: 1,
		WarningRevision: "warning-1", PolicyRevision: "policy-1",
		SyncRevision: 1, RemoteRevision: "head", UpdatedAt: now,
	}
	if err := validateConfigStatus(valid, 10, now); err != nil {
		t.Fatalf("valid status error = %v", err)
	}
	tests := []ConfigStatusReport{
		func() ConfigStatusReport { value := valid; value.SyncRevision = 0; return value }(),
		func() ConfigStatusReport { value := valid; value.UpdatedAt = now.Add(-6 * time.Minute); return value }(),
		func() ConfigStatusReport {
			value := valid
			value.Skipped = []ConfigStatusPath{{Path: "/Users/alice/.secret", Reason: "excluded"}}
			return value
		}(),
		func() ConfigStatusReport { value := valid; value.FencingToken = 2; return value }(),
		func() ConfigStatusReport {
			value := valid
			value.State, value.ErrorCode = "sync_uncertain", "network_error"
			return value
		}(),
		func() ConfigStatusReport { value := valid; value.State = "conflict"; return value }(),
	}
	for i, report := range tests {
		if err := validateConfigStatus(report, 10, now); !errors.Is(err, ErrConfigStatusInvalid) {
			t.Fatalf("case %d error = %v", i, err)
		}
	}
}

func TestConfigStatusValidationAcceptsHelperWarningWithSafeSkippedSpecialFiles(t *testing.T) {
	now := time.Date(2026, 7, 24, 7, 32, 39, 0, time.UTC)
	report := ConfigStatusReport{
		State: "revoked", Mode: ConfigModePullOnly, RepositoryID: "cfgrepo_fixture", AssignmentID: "cfgasn_fixture",
		EnvironmentID: "environment_fixture", MachineID: "helper_fixture", InstallationGeneration: 1,
		WarningRevision: "machine-a", PolicyRevision: "1", SyncRevision: 12,
		RemoteRevision: "b08059618b070ee1673a0db2301c36ca7696e07a",
		LastAttemptAt:  &now, LastSuccessfulAt: &now, UpdatedAt: now,
		Skipped: []ConfigStatusPath{{
			Path: ".paperboat/helper/runtime/pts_fixture/client.sock", Reason: "special_file",
		}},
		ErrorCode: "credential_expired",
	}
	if err := validateConfigStatus(report, 50, now); err != nil {
		t.Fatalf("helper status rejected: %v", err)
	}
}

func TestConfigStatusRejectionClassIsBoundedAndNonSecret(t *testing.T) {
	tests := map[error]string{
		errors.Join(ErrConfigStatusInvalid, errConfigStatusReport):          "report",
		errors.Join(ErrConfigStatusInvalid, errConfigStatusIdentity):        "identity",
		errors.Join(ErrConfigStatusInvalid, errConfigStatusHelper):          "helper_binding",
		errors.Join(ErrConfigStatusInvalid, errConfigStatusAssign):          "assignment_binding",
		errors.Join(errors.New("database details"), errConfigStatusPersist): "persistence",
		ErrConfigStatusStale:                      "stale",
		errors.New("unexpected sensitive detail"): "unknown",
	}
	for err, want := range tests {
		if got := ConfigStatusRejectionClass(err); got != want {
			t.Fatalf("class(%v) = %q, want %q", err, got, want)
		}
	}
}

func TestConfigAccountStatusUsesCanonicalAssignmentsConsentAndStatus(t *testing.T) {
	store := openControlPlaneTestDB(t)
	ctx := context.Background()
	suffix := strings.ReplaceAll(t.Name(), "/", "_")
	user := "status_user_" + suffix
	machineAEnvironment := "status_machine_a_" + suffix
	machineEnvironment := "status_machine_" + suffix
	repository := "status_repo_" + suffix
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	machineAHelper, machineHelper := "helper_machine_a_"+suffix, "helper_machine_"+suffix
	machineAAssignment, machineAssignment := "assignment_machine_a_"+suffix, "assignment_machine_"+suffix
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, []any{user, "workos_" + user, user + "@example.test"}},
		{`INSERT INTO paperboat.control_config_repositories (id,owner_user_id,provider,external_ref,display_name,state) VALUES ($1,$2,'github',$1,'owner/config','active')`, []any{repository, user}},
		{`INSERT INTO paperboat.control_environments (id,workspace_id,owner_user_id) VALUES ($1,$1,$3),($2,$2,$3)`, []any{machineAEnvironment, machineEnvironment, user}},
		{`INSERT INTO paperboat.control_helpers (id,environment_id,state,generation) VALUES ($1,$2,'active',1),($3,$4,'active',1)`, []any{machineAHelper, machineAEnvironment, machineHelper, machineEnvironment}},
		{`INSERT INTO paperboat.user_machines (id,user_id,environment_id,platform,architecture,workspace_root,state) VALUES ($1,$3,$4,'linux','unknown','/workspace','online'),($2,$3,$5,'darwin','arm64','/Users/test','online')`, []any{"machine_a_machine_" + suffix, "machine_" + suffix, user, machineAEnvironment, machineEnvironment}},
		{`INSERT INTO paperboat.control_config_assignments (id,machine_id,environment_id,repository_id,consent_state,warning_revision,accepted_at,configuration_revision) VALUES ($1,$2,$3,$4,'accepted','warning-1',now(),repeat('a',64)),($5,$6,$7,$4,'pending','warning-1',NULL,repeat('a',64))`, []any{machineAAssignment, "machine_a_machine_" + suffix, machineAEnvironment, repository, machineAssignment, "machine_" + suffix, machineEnvironment}},
		{`INSERT INTO paperboat.control_config_sync_statuses
		  (environment_id,repository_id,assignment_id,machine_id,installation_generation,
		   warning_revision,policy_revision,sync_revision,state,
		   pending_clean_path_count,machine_updated_at,observed_at)
		  VALUES ($1,$2,$3,$4,1,'warning-1','policy-1',2,'healthy',0,$5,$5)`,
			[]any{machineAEnvironment, repository, machineAAssignment, "machine_a_machine_" + suffix, now}},
	}
	for _, statement := range statements {
		if _, err := store.SQL().ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, user)
	})
	service := NewConfigStatusService(store, nil, 10)
	service.clock = func() time.Time { return now }
	service.SetAccountPolicy(config.ConfigSync{
		Mode:           "leased_writes",
		PolicyRevision: "policy-1", MaxFileBytes: 10, MaxBatchBytes: 20,
		ManifestContract: "paperboat-manifest-v1", ManifestMaxBytes: 1024,
		ManifestMaxLines: 10, ManifestMaxPatternBytes: 128,
		StaleHeartbeatAfter: time.Minute,
	})
	status, err := service.Account(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "consent_required" || len(status.Environments) != 2 {
		t.Fatalf("status = %#v", status)
	}
	for _, test := range []struct {
		state string
		total int
	}{{"", 2}, {"healthy", 1}, {"consent_required", 1}, {"conflict", 0}, {"configured", 2}, {"attention", 1}, {"settled", 1}} {
		page, err := service.AccountInventory(ctx, user, ConfigStatusInventoryOptions{Limit: 1, State: test.state})
		if err != nil || page.Pagination["total"] != test.total || len(page.Environments) > 1 {
			t.Fatalf("status page state=%s total=%v err=%v", test.state, page.Pagination, err)
		}
		if test.total > 1 && page.Pagination["next_offset"] != 1 {
			t.Fatal("status continuation missing")
		}
		if group := map[string]string{"attention": "consent_required", "settled": "healthy"}[test.state]; group != "" && page.Environments[0].State != group {
			t.Fatalf("state=%s listed %s", test.state, page.Environments[0].State)
		}
		if counts := page.Pagination["counts"].(map[string]int); test.state == "configured" && (counts["healthy"] != 1 || counts["consent_required"] != 1) {
			t.Fatalf("configured counts %v", counts)
		}
		if len(page.Environments) > 0 && slices.Contains(ConfigStatusStates, test.state) && page.Environments[0].State != test.state {
			t.Fatal("filter applied after limit")
		}
	}
	// The cursor walks the list in alias order, one row per page, and every page keeps the whole total.
	var walked []string
	opts := ConfigStatusInventoryOptions{Limit: 1}
	for pages := 0; ; pages++ {
		page, err := service.AccountInventory(ctx, user, opts)
		if err != nil || page.Pagination["total"] != 2 || len(page.Environments) != 1 || pages > 2 {
			t.Fatalf("cursor page %d: %v rows, pagination %v, %v", pages, len(page.Environments), page.Pagination, err)
		}
		walked = append(walked, page.Environments[0].EnvironmentID)
		if page.Next == nil {
			break
		}
		opts.After = page.Next
	}
	if len(walked) != 2 || walked[0] == walked[1] {
		t.Fatalf("cursor walk %v, want both environments once", walked)
	}
	for _, environment := range []string{machineAEnvironment, machineEnvironment, "unrelated_environment"} {
		page, err := service.AccountInventory(ctx, user, ConfigStatusInventoryOptions{Limit: 1, EnvironmentID: environment})
		if err != nil {
			t.Fatal(err)
		}
		if environment == "unrelated_environment" {
			if len(page.Environments) != 0 || page.Pagination["total"] != 0 {
				t.Fatal("foreign exact status disclosed")
			}
		} else if len(page.Environments) != 1 || page.Environments[0].EnvironmentID != environment || page.Pagination["total"] != 1 {
			t.Fatalf("exact status=%#v", page)
		}
	}
	states := map[string]string{}
	for _, environment := range status.Environments {
		states[environment.EnvironmentID] = environment.State
	}
	if states[machineAEnvironment] != "healthy" || states[machineEnvironment] != "consent_required" {
		t.Fatalf("states = %#v", states)
	}
}

func TestConfigEnvironmentUsesMachineAlias(t *testing.T) {
	encoded, err := json.Marshal(ConfigAccountEnvironment{MachineID: "mch_1", EnvironmentID: "env_1", Alias: "studio", RepositoryName: "My Config"})
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["alias"] != "studio" || payload["repository_name"] != "My Config" {
		t.Fatalf("incorrect machine or repository metadata: %s", encoded)
	}
	if _, present := payload["display_name"]; present {
		t.Fatalf("obsolete machine name: %s", encoded)
	}
}

func TestEveryMachineStatusRequiresConsentAndOmitsCategory(t *testing.T) {
	row := dbsqlc.ListOwnedControlConfigSyncStatusRow{
		AssignmentID:     sql.NullString{String: "a", Valid: true},
		RepositoryID:     sql.NullString{String: "r", Valid: true},
		RepositoryState:  sql.NullString{String: "active", Valid: true},
		EnvironmentState: "active",
	}
	for _, state := range []string{"", "pending", "revoked"} {
		row.ConsentState = sql.NullString{String: state, Valid: true}
		if got := canonicalAccountState(row, time.Now(), time.Minute); got != "consent_required" {
			t.Fatalf("consent %q state = %q", state, got)
		}
	}
	encoded, err := json.Marshal(ConfigAccountStatus{Policy: ConfigAccountPolicy{Mode: "read_only"}, Environments: []ConfigAccountEnvironment{{MachineID: "m"}}})
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	var policy map[string]json.RawMessage
	if err := json.Unmarshal(payload["policy"], &policy); err != nil {
		t.Fatal(err)
	}
	if _, present := policy["byod_enabled"]; present {
		t.Fatal("obsolete category feature switch exposed")
	}
	var machines []map[string]json.RawMessage
	if err := json.Unmarshal(payload["environments"], &machines); err != nil {
		t.Fatal(err)
	}
	if _, present := machines[0]["profile"]; present {
		t.Fatal("obsolete machine category exposed")
	}
}
