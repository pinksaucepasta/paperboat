package resourceusage

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/workspace"
)

func assertTeamMemberHistory(t *testing.T, store *db.DB, service *Service, suffix, owner, member string, now time.Time) {
	t.Helper()
	t.Run("team member history", func(t *testing.T) {
		ctx := t.Context()
		team := "usage-team-" + strings.ReplaceAll(suffix, "_", "-")
		machine, client := "team_machine_"+suffix, "team_client_"+suffix
		ws := "team:" + team
		exec := func(query string, args ...any) {
			t.Helper()
			if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			for _, entry := range []struct {
				sql string
				key string
			}{
				{"DELETE FROM resource_bandwidth_events WHERE workspace_id=$1", ws},
				{"DELETE FROM native_usage_reporting_status WHERE machine_id=$1", machine},
				{"DELETE FROM native_usage_authorities WHERE machine_id=$1", machine},
				{"DELETE FROM user_machine_access_sessions WHERE user_machine_id=$1", machine},
				{"DELETE FROM user_machines WHERE id=$1", machine},
				{"DELETE FROM cli_client_sessions WHERE id=$1", client + owner},
				{"DELETE FROM cli_client_sessions WHERE id=$1", client + member},
				{"DELETE FROM team_members WHERE team_id=$1", team},
				{"DELETE FROM teams WHERE team_id=$1", team},
			} {
				if _, err := store.SQL().ExecContext(cleanup, entry.sql, entry.key); err != nil {
					t.Error(err)
				}
			}
		})
		exec("INSERT INTO teams(team_id,owner_account,generation) VALUES($1,$2,1)", team, owner)
		exec("INSERT INTO team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'admin',true),($1,$3,1,'member',true)", team, owner, member)
		exec("INSERT INTO user_machines(id,user_id,owner_team_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES($1,$2,$3,$4,'linux','amd64','/workspace','online','occupied',true,1)", machine, owner, team, "team_env_"+suffix)
		cases := []struct {
			actor string
			want  Bytes
		}{{owner, Bytes{Upload: 17, Download: 19}}, {member, Bytes{Upload: 23, Download: 29}}}
		for _, c := range cases {
			session := "team_access_" + c.actor
			exec("INSERT INTO cli_client_sessions(id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at) VALUES($1,$2,$1,'Usage','desktop','linux',ARRAY['account:read'],'active',$3,$3)", client+c.actor, c.actor, now)
			exec("INSERT INTO user_machine_access_sessions(credential_kind,id,user_machine_id,user_id,environment_id,cli_client_session_id,http_base_url,helper_terminal_session_id,expires_at,created_at,capabilities) VALUES('control_plane_signed',$1,$2,$3,$4,$5,'','fixture',$6,$7,ARRAY['terminal'])", session, machine, c.actor, "team_env_"+suffix, client+c.actor, now.Add(time.Hour), now.Add(-time.Minute))
			batch := NativeBatch{Reports: []NativeReport{{AccessSessionID: session, StreamID: "team-stream", Consumer: "terminal", Mode: "relay", IntervalStart: now, IntervalEnd: now.Add(20 * time.Second), Upload: c.want.Upload, Download: c.want.Download}}}
			receipt, err := service.RecordNative(ctx, machine, 1, batch)
			if err != nil || receipt.Accepted != 1 {
				t.Fatalf("team measurement: %+v %v", receipt, err)
			}
		}
		scope := workspace.Scope{ID: team, TeamID: team}
		for _, c := range cases {
			got, err := service.Get(ctx, owner, scope, Query{Start: now, End: now.Add(time.Minute), ActorAccountID: c.actor})
			if err != nil || got.Totals != c.want || len(got.Resources) != 1 || len(got.Series) != 1 || got.Series[0].Bytes != c.want || got.Resources[0].Bytes != c.want {
				t.Fatalf("member usage: totals=%+v resources=%d series=%d err=%v", got.Totals, len(got.Resources), len(got.Series), err)
			}
		}
		got, err := service.Get(ctx, member, scope, Query{Start: now, End: now.Add(time.Minute)})
		if err != nil || got.Totals != (Bytes{Upload: 40, Download: 48}) {
			t.Fatalf("team total=%+v err=%v", got.Totals, err)
		}
	})
}
