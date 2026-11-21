package selfhost

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// One scope holding many active installations never refuses another scope's enrollment: only the per-scope cap
// (256) applies, there is no server-wide total.
func TestEnrollmentCapIsPerScopeOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
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
	if err = db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	prefix := "cap_" + randomIdentity() + "_"
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
	}
	defer func() {
		_, _ = store.SQL().ExecContext(ctx, `DELETE FROM paperboat.selfhost_installations WHERE id LIKE $1`, prefix+"%")
		_, _ = store.SQL().ExecContext(ctx, `DELETE FROM paperboat.control_tunnel_nodes WHERE id LIKE $1`, prefix+"%")
	}()
	// 16385 active installations of one scope, fixtures inserted past the per-scope cap on purpose.
	exec(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,endpoint_host,endpoint_tcp_port,endpoint_quic_port,state,ready,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at)
SELECT $1||n,$1||n,'1.0','','e.example.test',1,2,'registered',false,'t','t',ARRAY['relay'],ARRAY['derp_quic','derp_wss'],1,0,now(),now()+interval '1 hour' FROM generate_series(1,16385) n`, prefix)
	exec(`INSERT INTO paperboat.selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id)
SELECT $1||n,$1||n,sha256(convert_to($1||n,'UTF8')),sha256(convert_to('r'||$1||n,'UTF8')),'\x00','n','relay','account',$1||'crowded','claim_x' FROM generate_series(1,16385) n`, prefix)
	newKey := func() string {
		key, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(key)
	}
	service := New(store, "selfhost-test-encryption-key-32-bytes-long")
	inspection := Inspection{InstallationKey: newKey(), Name: "n", EndpointHost: "e.example.test", ExpiresAt: time.Now().Add(time.Hour).Unix(),
		Components: []Component{{Capability: "relay", TCPPort: 443, QUICPort: 444, Region: "t", FailureDomain: "t", CapacityLimit: 1}}}
	withKey := func(i Inspection, key string) Inspection { i.InstallationKey = key; return i }
	if _, _, err := service.reserve(ctx, "actor", Scope{Kind: "account", ID: prefix + "other"}, inspection, make([]byte, 32), ""); err != nil {
		t.Fatalf("another scope's enrollment: %v", err)
	}
	if _, _, err := service.reserve(ctx, "actor", Scope{Kind: "account", ID: prefix + "crowded"}, withKey(inspection, newKey()), make([]byte, 32), ""); err == nil || !errors.Is(err, ErrLimit) {
		t.Fatalf("the crowded scope's own enrollment = %v, want ErrLimit", err)
	}
}
