package testutil

import (
	"database/sql"
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
