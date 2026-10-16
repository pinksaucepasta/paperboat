package resourceusage

import (
	"context"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

// Exercise the actual admission trigger and signed-machine reporting owner,
// including a personal host used inside a team workspace.
func assertNativeConsumerWorkspace(t *testing.T, store *db.DB, service *Service, suffix, owner, member string, now time.Time) {
	t.Helper()
	t.Run("all-native-consumers-selected-workspace", func(t *testing.T) {
		ctx := t.Context()
		team, machine, client := "native-team-"+suffix, "native_machine_"+suffix, "native_client_"+suffix
		exec := func(q string, args ...any) {
			t.Helper()
			if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
				t.Fatal(err)
			}
		}
		defer func() {
			for _, q := range []string{
				`DELETE FROM resource_bandwidth_events WHERE machine_id=$1`,
				`DELETE FROM native_usage_reporting_status WHERE machine_id=$1`,
				`DELETE FROM native_usage_authorities WHERE machine_id=$1`,
				`DELETE FROM user_machine_access_sessions WHERE user_machine_id=$1`,
				`DELETE FROM user_machines WHERE id=$1`,
			} {
				if _, err := store.SQL().ExecContext(context.Background(), q, machine); err != nil {
					t.Error(err)
				}
			}
			for _, entry := range []struct{ q, key string }{{`DELETE FROM cli_client_sessions WHERE id=$1`, client}, {`DELETE FROM teams WHERE team_id=$1`, team}} {
				if _, err := store.SQL().ExecContext(context.Background(), entry.q, entry.key); err != nil {
					t.Error(err)
				}
			}
		}()
		exec(`INSERT INTO teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
		exec(`INSERT INTO user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES($1,$2,$1,'linux','amd64','/workspace','online','occupied',true,1)`, machine, owner)
		exec(`INSERT INTO cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at) VALUES($1,$2,$1,'Usage','desktop','linux',ARRAY['account:read'],'active',$3,$3)`, client, member, now)
		for _, c := range []struct{ consumer, capability, kind string }{
			{"terminal", "terminal", "terminal"}, {"exec", "exec", "exec"}, {"ssh", "managed_ssh", "ssh"}, {"file", "files", "file_transfer"}, {"file_transfer", "files", "file_transfer"}, {"file_transfer_key", "files", "file_transfer"}, {"private_http", "private_access", "private_http"}, {"private_tcp", "private_access", "private_tcp"},
		} {
			t.Run(c.consumer, func(t *testing.T) {
				session := "native_" + c.consumer + suffix
				exec(`INSERT INTO user_machine_access_sessions(credential_kind,id,user_machine_id,user_id,environment_id,cli_client_session_id,http_base_url,helper_terminal_session_id,expires_at,created_at,capabilities,team_id) VALUES('control_plane_signed',$1,$2,$3,$2,$4,'','fixture',$5,$6,ARRAY[$7]::text[],$8)`, session, machine, member, client, now.Add(time.Hour), now.Add(-time.Minute), c.capability, team)
				report := NativeReport{AccessSessionID: session, StreamID: session, Consumer: c.consumer, Mode: "direct", IntervalStart: now, IntervalEnd: now.Add(20 * time.Second), Upload: 17, Download: 19}
				receipt, err := service.RecordNative(ctx, machine, 1, NativeBatch{Reports: []NativeReport{report}})
				if err != nil || receipt.Accepted != 1 {
					t.Fatalf("native admission/report: %+v %v", receipt, err)
				}
				var workspace, actor string
				var upload, download int64
				err = store.SQL().QueryRowContext(ctx, `SELECT workspace_id,actor_account_id,upload_bytes,download_bytes FROM resource_bandwidth_events WHERE machine_id=$1 AND resource_kind=$2`, machine, c.kind).Scan(&workspace, &actor, &upload, &download)
				if err != nil || workspace != "team:"+team || actor != member || upload != 17 || download != 19 {
					t.Fatalf("native workspace=%q actor=%q bytes=%d/%d: %v", workspace, actor, upload, download, err)
				}
				exec(`DELETE FROM resource_bandwidth_events WHERE machine_id=$1`, machine)
			})
		}
	})
}
