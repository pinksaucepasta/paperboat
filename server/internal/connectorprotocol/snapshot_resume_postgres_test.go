package connectorprotocol

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"testing"
)

func TestTRK08PostgresSnapshotSelectsPendingResumeAfterPausedActive(t *testing.T) {
	f := newTRK08PostgresFixture(t)
	ctx := context.Background()
	if _, err := f.database.SQL().ExecContext(ctx, `UPDATE paperboat.tunnel_config_generations SET activation_state='superseded',activated_at=NULL WHERE tunnel_id=$1`, f.tunnelID); err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(f.config.Payload, &value); err != nil {
		t.Fatal(err)
	}
	for _, generation := range []int{2, 3} {
		state := "pending"
		value["desired_state"] = "active"
		if generation == 2 {
			state = "active"
			value["desired_state"] = "paused"
		}
		value["generation"] = generation
		payload, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(payload)
		if _, err := f.database.SQL().ExecContext(ctx, `INSERT INTO paperboat.tunnel_config_generations(tunnel_id,generation,previous_generation,content_hash,snapshot,activation_state,created_by_actor_id,activated_at,retained_until) VALUES($1,$2,$2::bigint-1,$3,$4,$5,$6,CASE WHEN $5='active' THEN now() ELSE NULL END,now()+interval '24 hours')`, f.tunnelID, generation, digest[:], payload, state, f.accountID); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := f.store.Snapshot(ctx, f.tunnelID)
	if err != nil || snapshot.Generation != 3 {
		t.Fatalf("pending resume snapshot generation=%d error=%v", snapshot.Generation, err)
	}
	if err := json.Unmarshal(snapshot.Payload, &value); err != nil || value["desired_state"] != "active" {
		t.Fatalf("resume payload state=%v error=%v", value["desired_state"], err)
	}
}
