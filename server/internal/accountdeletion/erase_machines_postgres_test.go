package accountdeletion

import (
	"testing"
	"time"
)

// Deleting an account removes its machines a chunk at a time, not one by one: 200 machines, each with its own
// environment, go in one transaction, each leaves its audit event, and the whole thing stays within a time bound.
func TestAccountDeletionErasesManyMachinesSetBasedPostgres(t *testing.T) {
	f := newFixture(t)
	u := f.user("erase_many")
	const n = 200
	f.exec(`INSERT INTO user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation)
		SELECT $1||g, $2, $1||g, 'm-'||g, 'linux','amd64','/workspace','online','occupied',true,1 FROM generate_series(1,$3::int) g`, "many_"+u.ID+"_", u.ID, n)
	if got := f.count(`SELECT count(*) FROM user_machines WHERE user_id=$1 AND deleted_at IS NULL`, u.ID); got != n {
		t.Fatalf("seeded %d machines, want %d", got, n)
	}
	started := time.Now()
	if err := f.svc.Delete(t.Context(), u, time.Now(), u.PrimaryEmail); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(started); took > 30*time.Second {
		t.Fatalf("deleting %d machines took %v, want a bounded set-based run", n, took)
	}
	for name, query := range map[string]string{
		"machines still live":      `SELECT count(*) FROM user_machines WHERE user_id=$1 AND deleted_at IS NULL`,
		"machine rows not deleted": `SELECT count(*) FROM user_machines WHERE user_id=$1 AND state<>'deleted'`,
		"seats still occupied":     `SELECT count(*) FROM user_machines WHERE user_id=$1 AND seat_state<>'released'`,
		"machines still online":    `SELECT count(*) FROM user_machines WHERE user_id=$1 AND online`,
		"active control sessions":  `SELECT count(*) FROM user_machine_access_sessions WHERE user_id=$1 AND state='active'`,
	} {
		if got := f.count(query, u.ID); got != 0 {
			t.Errorf("%s: %d, want 0", name, got)
		}
	}
	if got := f.count(`SELECT count(*) FROM audit_events WHERE event_type='user_machine.deleted' AND resource_id LIKE $1`, "many_"+u.ID+"_%"); got != n {
		t.Errorf("user_machine.deleted audit events = %d, want %d", got, n)
	}
}
