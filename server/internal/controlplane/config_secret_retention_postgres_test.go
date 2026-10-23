package controlplane

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// Expired config-sync credentials and repository-access tokens lose their ciphertext at expiry and the
// row an hour later (audit D2). Live rows, and unexpired rows still waiting for provider revocation, keep it.
func TestExpiredConfigSecretsAreClearedThenDeletedOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL required")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if err = db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	x := fmt.Sprint(now.UnixNano())
	user, machine, repository := "usr_secret_"+x, "um_secret_"+x, "repo_secret_"+x
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := database.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { database.SQL().ExecContext(ctx, `DELETE FROM paperboat.users WHERE id=$1`, user) })
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, user)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online) VALUES($1,$2,$3,'linux','amd64','/w','online','occupied',true)`, machine, user, "env_secret_"+x)
	exec(`INSERT INTO paperboat.control_config_repositories(id,owner_user_id,provider,external_ref,display_name) VALUES($1,$2,'github',$1,'fixture')`, repository, user)
	credential := func(name string, expires time.Time) string {
		jti := "jti_" + name + "_" + x
		exec(`INSERT INTO paperboat.control_config_credentials(jti,operation_key,request_hash,environment_id,machine_id,assignment_id,credential_ciphertext,expires_at) VALUES($1,$1,'\x01',$2,$3,'assign',$4,$5)`, jti, "env_secret_"+x, machine, []byte("sealed"), expires)
		return jti
	}
	access := func(name, state string, expires, revoked any, created time.Time) string {
		id := "op_" + name + "_" + x
		var ciphertext any
		if state == "issued" {
			ciphertext = []byte("sealed")
		}
		exec(`INSERT INTO paperboat.control_config_repository_access_operations(operation_id,request_hash,repository_id,assignment_id,environment_id,machine_id,installation_generation,warning_revision,state,access_ciphertext,expires_at,revoked_at,created_at) VALUES($1,'\x01',$2,'assign',$3,$4,1,'w',$5,$6,$7,$8,$9)`, id, repository, "env_secret_"+x, machine, state, ciphertext, expires, revoked, created)
		return id
	}
	liveCredential := credential("live", now.Add(time.Minute))
	expiredCredential := credential("expired", now.Add(-30*time.Minute))
	oldCredential := credential("old", now.Add(-2*time.Hour))
	liveAccess := access("live", "issued", now.Add(time.Minute), nil, now)
	pendingRevoke := access("pending_revoke", "issued", now.Add(time.Minute), now.Add(-time.Second), now)
	expiredAccess := access("expired", "issued", now.Add(-30*time.Minute), nil, now.Add(-time.Hour))
	oldAccess := access("old", "issued", now.Add(-2*time.Hour), nil, now.Add(-3*time.Hour))
	stalePending := access("stale_pending", "pending", nil, nil, now.Add(-48*time.Hour))
	freshPending := access("fresh_pending", "pending", nil, nil, now)

	service := &ConfigRepositoryAccessService{store: database, clock: func() time.Time { return now }}
	if err := service.pruneExpiredConfigSecrets(ctx); err != nil {
		t.Fatal(err)
	}
	ciphertext := func(table, key, id string) (present, exists bool) {
		t.Helper()
		column := map[string]string{"control_config_credentials": "credential_ciphertext", "control_config_repository_access_operations": "access_ciphertext"}[table]
		var value []byte
		err := database.SQL().QueryRowContext(ctx, `SELECT `+column+` FROM paperboat.`+table+` WHERE `+key+`=$1`, id).Scan(&value)
		if err == sql.ErrNoRows {
			return false, false
		}
		if err != nil {
			t.Fatal(err)
		}
		return value != nil, true
	}
	for _, check := range []struct {
		name, table, key, id string
		present, exists      bool
	}{
		{"live credential", "control_config_credentials", "jti", liveCredential, true, true},
		{"expired credential", "control_config_credentials", "jti", expiredCredential, false, true},
		{"credential past grace", "control_config_credentials", "jti", oldCredential, false, false},
		{"live access", "control_config_repository_access_operations", "operation_id", liveAccess, true, true},
		{"unexpired access pending provider revoke", "control_config_repository_access_operations", "operation_id", pendingRevoke, true, true},
		{"expired access", "control_config_repository_access_operations", "operation_id", expiredAccess, false, true},
		{"access past grace", "control_config_repository_access_operations", "operation_id", oldAccess, false, false},
		{"never-issued access past retention", "control_config_repository_access_operations", "operation_id", stalePending, false, false},
		{"fresh pending access", "control_config_repository_access_operations", "operation_id", freshPending, false, true},
	} {
		if present, exists := ciphertext(check.table, check.key, check.id); present != check.present || exists != check.exists {
			t.Errorf("%s: ciphertext present=%v row exists=%v, want %v/%v", check.name, present, exists, check.present, check.exists)
		}
	}
}
