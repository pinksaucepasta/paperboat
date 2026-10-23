package peeridentity

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	wire "github.com/pinksaucepasta/paperboat/shared/peeridentity"
)

// Every machine stores the account trust set, one key per active endpoint. On the shared test account it
// reached 92 keys because CLI sessions that could no longer refresh, and the pairing sessions of machines
// disconnected before 91aeec3d, still counted; the set outgrew what clients stored. A session counts only
// while it can refresh, rotation does not add keys, and migration 118 ends removed machines' sessions.
func TestTransportKeysCountOnlySessionsThatCanStillRefresh(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PAPERBOAT_TEST_DATABASE_DSN to run peer identity repository integration tests")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	suffix := strings.ToLower(t.Name()) + strings.ReplaceAll(now.Format("150405.000000000"), ".", "")
	userID, removedMachineID := "peer_trust_user_"+suffix, "peer_trust_removed_"+suffix
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.users (id,workos_subject,primary_email,status) VALUES ($1,$2,$3,'active')`, userID, "workos_"+userID, userID+"@example.test")
	exec(`INSERT INTO paperboat.user_machines (id,user_id,environment_id,platform,architecture,workspace_root,state,online,public_identity_key) VALUES ($1,$2,$3,'linux','amd64','/home/test','disconnected',false,$4)`, removedMachineID, userID, "env_"+suffix, "identity_"+suffix)
	repository, err := NewSQLRepository(store, audit.NewWriter(store))
	if err != nil {
		t.Fatal(err)
	}
	keyOf := map[string]string{}
	session := func(name string, machineID any, refreshExpiries ...time.Duration) {
		t.Helper()
		id := "peer_trust_" + name + "_" + suffix
		exec(`INSERT INTO paperboat.cli_client_sessions (id,user_id,client_id,client_label,device_type,os,scopes,state,created_at,approved_at,user_machine_id) VALUES ($1,$2,$3,$4,'desktop','linux',ARRAY['machines:connect'],'active',$5,$5,$6)`, id, userID, "client_"+id, name, now.Add(-time.Hour), machineID)
		for index, expiry := range refreshExpiries {
			state := "active"
			if index < len(refreshExpiries)-1 {
				state = "rotated"
			}
			exec(`INSERT INTO paperboat.cli_refresh_tokens (token_hash,cli_client_session_id,state,expires_at,created_at) VALUES ($1,$2,$3,$4,$5)`, "hash_"+id+"_"+state+string(rune('a'+index)), id, state, now.Add(expiry), now.Add(-time.Hour))
		}
		public, private, _ := ed25519.GenerateKey(nil)
		raw := signedFixture(t, private, userID, wire.RoleCLI, id, 1, 1, now.Add(-time.Minute), now.Add(time.Hour))
		certificate, err := wire.Verify(raw, public, wire.Expected{AccountID: userID, Role: wire.RoleCLI, EndpointID: id, Generation: 1, Serial: 1}, now)
		if err != nil {
			t.Fatal(err)
		}
		fingerprint := sha256.Sum256(public)
		certificate.KeyID, certificate.RootFingerprint = keyIDForFingerprint(fingerprint), fingerprint
		if _, err := repository.BootstrapFresh(ctx, "operation_trust_"+id, userID, id, public, certificate, now); err != nil {
			t.Fatal(err)
		}
		keyOf[name] = certificate.KeyID
	}
	day := 24 * time.Hour
	session("live", nil, 30*day)
	session("rotated", nil, -day, 29*day)
	session("idle", nil, -day)
	session("removed_pairing", removedMachineID, 30*day)
	expect := func(step string, names ...string) {
		t.Helper()
		root, err := repository.ResolveTransportKeys(ctx, userID, now)
		got := map[string]bool{}
		for _, key := range root.Keys {
			got[key.KeyID] = true
		}
		if err != nil || len(root.Keys) != len(names) {
			t.Fatalf("%s: %d keys, want %v: err=%v", step, len(root.Keys), names, err)
		}
		for _, name := range names {
			if !got[keyOf[name]] {
				t.Fatalf("%s: %s missing from %d keys", step, name, len(root.Keys))
			}
		}
	}
	expect("before cleanup", "live", "rotated", "removed_pairing")

	paths, err := filepath.Glob("../db/migrations/*_end_removed_machine_cli_sessions.sql")
	if err != nil || len(paths) != 1 {
		t.Fatalf("migration=%v err=%v", paths, err)
	}
	body, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	up, _, _ := strings.Cut(string(body), "-- +goose Down")
	exec(up)
	expect("after cleanup", "live", "rotated")
	var state string
	if err := store.SQL().QueryRowContext(ctx, `SELECT state FROM paperboat.cli_client_sessions WHERE id=$1`, "peer_trust_removed_pairing_"+suffix).Scan(&state); err != nil || state != "revoked" {
		t.Fatalf("removed machine's pairing session state=%q err=%v", state, err)
	}
}
