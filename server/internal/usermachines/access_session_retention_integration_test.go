package usermachines

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/audit"
	"github.com/pinksaucepasta/paperboat-server/internal/db/dbsqlc"
)

// Expired access sessions must not stay active forever: after the retention period they are
// retired, while live and recently expired ones (which a client may still name as admitted) stay.
func TestExpiredAccessSessionsAreRetiredAfterTheRetentionPeriod(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	user, machine, cli := "retire_user_"+suffix, "retire_machine_"+suffix, "retire_cli_"+suffix
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.user_machine_access_sessions WHERE user_id=$1`, user)
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.cli_client_sessions WHERE user_id=$1`, user)
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.user_machines WHERE user_id=$1`, user)
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.users WHERE id=$1`, user)
	})
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES($1,$2,$3,'linux','amd64','/w','online','occupied',true,1)`, machine, user, "env_"+machine)
	exec(`INSERT INTO paperboat.cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at) VALUES($1,$2,'client','retire','desktop','test',ARRAY['machines:connect'],'active',now(),now())`, cli, user)
	now := time.Now().UTC()
	sessions := map[string]time.Duration{"retire_live_" + suffix: time.Minute, "retire_recent_" + suffix: -time.Hour, "retire_old_" + suffix: -30 * 24 * time.Hour}
	for id, expiresIn := range sessions {
		if err := store.Queries().CreateUserMachineAccessSession(ctx, dbsqlc.CreateUserMachineAccessSessionParams{
			CredentialKind: "control_plane_signed", ID: id, UserMachineID: machine, UserID: user, EnvironmentID: "env_" + machine,
			CLIClientSessionID: cli, HttpBaseUrl: "https://ingress.example.test", HelperTerminalSessionID: "helper_" + id,
			ExpiresAt: now.Add(expiresIn), Capabilities: []string{"exec"}, OperationID: "operation_" + id,
		}); err != nil {
			t.Fatal(err)
		}
	}
	service := New(store, audit.NewWriter(store), Policy{ExpiredAccessSessionRetention: 7 * 24 * time.Hour}, nil)
	service.now = func() time.Time { return now }
	if err := service.retireExpiredAccessSessions(ctx); err != nil {
		t.Fatal(err)
	}
	state := func(id string) string {
		var value string
		if err := store.SQL().QueryRowContext(ctx, `SELECT state FROM paperboat.user_machine_access_sessions WHERE id=$1`, id).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	if state("retire_live_"+suffix) != "active" || state("retire_recent_"+suffix) != "active" || state("retire_old_"+suffix) != "revoked" {
		t.Fatalf("states live=%s recent=%s old=%s", state("retire_live_"+suffix), state("retire_recent_"+suffix), state("retire_old_"+suffix))
	}
}
