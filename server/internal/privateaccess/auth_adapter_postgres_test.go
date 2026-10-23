package privateaccess

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

func TestAdmittedMachineIdentitySurvivesCredentialExpiryAndObservationLossUntilRevoked(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL DSN required")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if err := db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	suffix := fmt.Sprint(time.Now().UnixNano())
	account, machine, environment, jti := "actor_"+suffix, "machine_"+suffix, "environment_"+suffix, "identity_"+suffix
	execute := func(query string, args ...any) {
		t.Helper()
		if _, err := database.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	execute(`INSERT INTO users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, account, account+"@example.test")
	defer database.SQL().ExecContext(context.Background(), `DELETE FROM users WHERE id=$1`, account)
	execute(`INSERT INTO control_environments(id,workspace_id,owner_user_id) VALUES($1,$1,$2)`, environment, account)
	execute(`INSERT INTO user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,online,installation_generation) VALUES($1,$2,$3,'linux','amd64','/workspace','online','occupied',true,1)`, machine, account, environment)
	execute(`INSERT INTO machine_control_renewals(operation_id,machine_id,installation_generation,credential_jti,issued_at,expires_at,session_generation) VALUES($1,$2,1,$1,$3,$4,1)`, jti, machine, now, now.Add(time.Second))
	execute(`INSERT INTO machine_control_sessions(machine_id,installation_generation,session_generation,operation_id,credential_jti,issued_at,expires_at) VALUES($1,1,1,$2,$2,$3,$4)`, machine, jti, now, now.Add(time.Second))
	verifier, err := NewSQLMachineStateVerifier(database)
	if err != nil {
		t.Fatal(err)
	}
	identity := Identity{AccountID: account, UserID: account, MachineID: machine, SessionID: jti, InstallationGeneration: 1, ExpiresAt: now.Add(time.Second), Method: "machine"}
	if err := verifier.VerifyCurrentMachine(ctx, identity, now); err != nil {
		t.Fatal(err)
	}
	later := now.Add(2 * time.Second)
	if err := verifier.VerifyCurrentMachine(ctx, identity, later); err == nil {
		t.Fatal("expired credential admitted new work")
	}
	if err := verifier.VerifyAdmittedMachine(ctx, identity, later); err != nil {
		t.Fatal("admitted identity ended at credential TTL", err)
	}
	execute(`UPDATE user_machines SET state='offline',online=false WHERE id=$1`, machine)
	if err := verifier.VerifyAdmittedMachine(ctx, identity, later); err != nil {
		t.Fatal("observation loss falsely revoked admitted identity", err)
	}
	for _, wrong := range []Identity{
		{AccountID: "foreign_actor", MachineID: machine, SessionID: jti, InstallationGeneration: 1},
		{AccountID: account, MachineID: machine, SessionID: jti, InstallationGeneration: 2},
	} {
		if err := verifier.VerifyAdmittedMachine(ctx, wrong, later); err == nil {
			t.Fatal("admitted identity lost exact account/install binding")
		}
	}
	execute(`UPDATE user_machines SET revoked_at=now() WHERE id=$1`, machine)
	if err := verifier.VerifyAdmittedMachine(ctx, identity, later); err == nil {
		t.Fatal("explicitly revoked machine retained admitted identity")
	}
}
