package resourceusage

import (
	"context"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
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
			{"terminal", "terminal", "terminal"}, {"exec", "exec", "exec"}, {"ssh", "managed_ssh", "ssh"}, {"file_transfer", "files", "file_transfer"}, {"private_http", "private_access", "private_http"}, {"private_tcp", "private_access", "private_tcp"},
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
		// A host meters every stream, recordings included: a recording report is judged on its own (here: no usage
		// authority, so rejected) and never refuses the rest of the batch, which it did as an unknown consumer.
		terminal := NativeReport{AccessSessionID: "native_terminal" + suffix, StreamID: "batch_terminal", Consumer: "terminal", Mode: "direct", IntervalStart: now, IntervalEnd: now.Add(20 * time.Second), Upload: 1, Download: 1}
		recording := terminal
		recording.StreamID, recording.Consumer = "batch_recording", "recording"
		receipt, err := service.RecordNative(ctx, machine, 1, NativeBatch{Reports: []NativeReport{terminal, recording}})
		if err != nil || receipt.Accepted != 1 || len(receipt.RejectedIndices) != 1 || receipt.RejectedIndices[0] != 1 {
			t.Fatalf("a batch with a recording report: %+v %v", receipt, err)
		}
		exec(`DELETE FROM resource_bandwidth_events WHERE machine_id=$1`, machine)
	})
}

// A stream the host admitted from its own rules (every managed SSH stream, D1) has no access session. Its report
// names the host as the session and the peer the native session proved; it is recorded under the consumer's kind in
// the workspace the peer's access decides, with the exact byte counts. A peer without access, or a report naming
// another resource, records nothing.
func assertHostRulesUsage(t *testing.T, store *db.DB, service *Service, suffix, owner, stranger string, now time.Time) {
	t.Helper()
	t.Run("host-rules-ssh", func(t *testing.T) {
		ctx := t.Context()
		machine := "rules_machine_" + suffix
		exec := func(q string, args ...any) {
			t.Helper()
			if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
				t.Fatal(err)
			}
		}
		defer func() {
			for _, q := range []string{`DELETE FROM resource_bandwidth_events WHERE machine_id=$1`, `DELETE FROM native_usage_reporting_status WHERE machine_id=$1`, `DELETE FROM user_machines WHERE id=$1`} {
				if _, err := store.SQL().ExecContext(context.Background(), q, machine); err != nil {
					t.Error(err)
				}
			}
		}()
		exec(`INSERT INTO user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES($1,$2,$1,'linux','amd64','/workspace','online','occupied',true,1)`, machine, owner)
		report := func(session, peer, stream string) NativeReport {
			return NativeReport{AccessSessionID: session, PeerAccountID: peer, StreamID: stream, Consumer: "ssh", Mode: "relay", IntervalStart: now, IntervalEnd: now.Add(20 * time.Second), Upload: 1 << 20, Download: 1<<20 + 7}
		}
		receipt, err := service.RecordNative(ctx, machine, 1, NativeBatch{Reports: []NativeReport{report(machine, owner, "s1"), report(machine, stranger, "s2"), report("other_"+machine, owner, "s3")}})
		if err != nil || receipt.Accepted != 1 || len(receipt.RejectedIndices) != 2 || receipt.RejectedIndices[0] != 1 || receipt.RejectedIndices[1] != 2 {
			t.Fatalf("host-rules reports: %+v %v", receipt, err)
		}
		var workspace, actor, kind string
		var upload, download int64
		var events int
		err = store.SQL().QueryRowContext(ctx, `SELECT count(*) OVER (),workspace_id,actor_account_id,resource_kind,upload_bytes,download_bytes FROM resource_bandwidth_events WHERE machine_id=$1`, machine).Scan(&events, &workspace, &actor, &kind, &upload, &download)
		if err != nil || events != 1 || kind != "ssh" || workspace != "personal:"+owner || actor != owner || upload != 1<<20 || download != 1<<20+7 {
			t.Fatalf("host-rules event count=%d kind=%q workspace=%q actor=%q bytes=%d/%d: %v", events, kind, workspace, actor, upload, download, err)
		}
		if err := (NativeReport{AccessSessionID: machine, PeerAccountID: owner, StreamID: "s4", Consumer: "file_transfer", Mode: "relay", IntervalStart: now, IntervalEnd: now}).validate(now); err == nil {
			t.Fatal("a host-rules report for a consumer host rules never admit was valid")
		}
	})
}
