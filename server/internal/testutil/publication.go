package testutil

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"testing"
)

// PublicationMachine supplies the parent host for fixtures that exercise
// publication state without enrolling or running a host. One account may have
// several publications on this same machine.
func PublicationMachine(t testing.TB, store *sql.DB, account string) string {
	t.Helper()
	id := "publication_host_" + account
	if _, err := store.ExecContext(t.Context(), `INSERT INTO paperboat.user_machines
 (id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state)
 VALUES($1,$2,$1,'linux','amd64','/workspace','offline','released') ON CONFLICT(id) DO NOTHING`, id, account); err != nil {
		t.Fatal(err)
	}
	return id
}

// VerifiedWorkspaceDomain adds hostname as a verified domain of a workspace (team "" is the account's
// personal workspace), so tunnel and preview hostnames one level under it can be bound.
func VerifiedWorkspaceDomain(t testing.TB, store *sql.DB, account, team, hostname string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(hostname))
	id := "wsd_" + hex.EncodeToString(sum[:12])
	if _, err := store.ExecContext(t.Context(), `INSERT INTO paperboat.workspace_domains
 (id,account_id,owner_team_id,hostname,challenge_reference,ownership_state,verified_at,created_by_actor_id)
 VALUES($1,$2,NULLIF($3,''),$4,'account:'||$2,'verified',now(),$2) ON CONFLICT(hostname) DO NOTHING`, id, account, team, hostname); err != nil {
		t.Fatal(err)
	}
	return id
}
