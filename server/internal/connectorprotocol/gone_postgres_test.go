package connectorprotocol

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"os"
	"strings"
	"testing"
	"time"

	wire "github.com/pinksaucepasta/paperboat/shared/connectorprotocol"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/testutil"
)

// A connector of an expired tunnel, or on a revoked machine, is gone: the host must stop asking
// for carriers instead of retrying a 503 forever (prod 2026-10-23: ~41 bootstrap 503/min).
func TestSQLConnectorGoneCoversExpiredTunnelsAndRevokedMachinesOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	ctx := context.Background()
	if err := db.Migrate(ctx, database); err != nil {
		t.Fatal(err)
	}
	suffix := strings.ReplaceAll(time.Now().UTC().Format("150405.000000000"), ".", "")
	account, host, tunnel, connector := "acct_gone_"+suffix, "mch_gone_"+suffix, "tunnel_gone_"+suffix, "con_gone_"+suffix
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := database.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	defer func() { _, _ = database.SQL().ExecContext(ctx, `DELETE FROM paperboat.users WHERE id=$1`, account) }()
	key := sha256.Sum256([]byte(host))
	exec(`INSERT INTO paperboat.users (id, workos_subject, primary_email, status) VALUES ($1, $1, $2, 'active')`, account, account+"@example.test")
	exec(`INSERT INTO paperboat.user_machines (id, user_id, environment_id, description, platform, architecture, workspace_root, state, seat_state, public_identity_key)
VALUES ($1, $2, $3, 'gone test', 'linux', 'amd64', '/workspace', 'online', 'occupied', $4)`, host, account, "env_"+host, base64.RawURLEncoding.EncodeToString(key[:]))
	endpointID := testutil.EndpointID("gone:" + suffix)
	exec(`INSERT INTO paperboat.tunnels (id, account_id, name, desired_state, access_mode, generation, stable_endpoint_id, stable_endpoint, created_by_host_id, created_by_actor_id, summary_code, created_at, updated_at, expires_at)
VALUES ($1, $2, $3, 'active', 'public', 1, $4, $5, $6, $2, 'pending', now()-interval '2 hours', now()-interval '2 hours', now()+interval '1 hour')`, tunnel, account, "gone-"+suffix, endpointID, "https://"+testutil.EndpointLabel(endpointID)+".tunnels.example.test", host)
	exec(`INSERT INTO paperboat.tunnel_connectors (id, tunnel_id, host_id, credential_reference, credential_thumbprint, rotation_generation, desired_state, protocol_version, last_applied_config_generation, drain_state, generation, created_at, updated_at)
VALUES ($1, $2, $3, $4, 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA', 1, 'active', $5, 0, 'accepting', 1, now(), now())`, connector, tunnel, host, "keychain://paperboat/gone/"+suffix, wire.ProtocolVersion)
	gone := SQLConnectorGone(database)
	check := func(want bool, what string) {
		t.Helper()
		got, err := gone(ctx, tunnel, connector)
		if err != nil || got != want {
			t.Fatalf("%s: gone=%v err=%v, want %v", what, got, err, want)
		}
	}
	check(false, "live tunnel")
	exec(`UPDATE paperboat.tunnels SET expires_at=now()-interval '1 second' WHERE id=$1`, tunnel)
	check(true, "expired tunnel")
	exec(`UPDATE paperboat.tunnels SET expires_at=NULL WHERE id=$1`, tunnel)
	check(false, "tunnel without expiry")
	exec(`UPDATE paperboat.user_machines SET revoked_at=now() WHERE id=$1`, host)
	check(true, "revoked host machine")
	if got, err := gone(ctx, tunnel, "con_unknown_"+suffix); err != nil || !got {
		t.Fatalf("unknown connector gone=%v err=%v", got, err)
	}
}
