package peersessions

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// An engine reports its own candidate endpoints with its configuration request (PLAN 3b, N5, step 1): the answer
// says the server keeps them (endpoint_reports), the normalized set is stored on the identity, a request without
// endpoints keeps the stored set, a new set replaces it, and storing moves no peer_network topic yet.
func TestReportedEndpointsAreStoredOnTheIdentityOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	s := fmt.Sprint(time.Now().UnixNano())
	account := "endpoints_user_" + s
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%v\n%s", err, query)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, account)
	net := sha256.Sum256([]byte(s))
	exec(`INSERT INTO paperboat.control_environments(id,workspace_id,owner_user_id,desired_state,desired_version,applied_state,applied_version,created_at,updated_at)
SELECT $1||'_env_'||g,$1||'_env_'||g,$1,'active',1,'ready',1,now(),now() FROM generate_series(0,1) g`, account)
	exec(`INSERT INTO paperboat.user_machines(id,user_id,environment_id,alias,platform,architecture,workspace_root,state,seat_state,online,installation_generation)
SELECT $1||'_m_'||g,$1,$1||'_env_'||g,'m-'||g,'linux','amd64','/w','online','occupied',true,1 FROM generate_series(0,1) g`, account)
	exec(`INSERT INTO paperboat.peer_endpoint_certificates(fingerprint,user_id,endpoint_id,role,generation,serial,certificate,quic_public_key,issued_at,expires_at,key_id,signer_public_key)
SELECT sha256(convert_to($1||'_m_'||g,'UTF8')),$1,$1||'_m_'||g,'machine',1,1,decode(repeat('00',200),'hex'),sha256(convert_to('key'||$1||'_m_'||g,'UTF8')),now()-interval '1 minute',now()+interval '1 day','key',
 sha256(convert_to('key'||$1||'_m_'||g,'UTF8')) FROM generate_series(0,1) g`, account)
	exec(`INSERT INTO paperboat.peer_network_identities(user_id,endpoint_id,role,machine_id,endpoint_generation,machine_generation,key_generation,
wireguard_public_key,quic_certificate_fingerprint,virtual_address,created_at,updated_at)
SELECT $1,$1||'_m_'||g,'machine',$1||'_m_'||g,1,1,1,sha256(convert_to('key'||$1||'_m_'||g,'UTF8')),sha256(convert_to($1||'_m_'||g,'UTF8')),
 ('100.'||(64+$2::int)||'.'||$3::int||'.'||(1+g))::inet,now(),now() FROM generate_series(0,1) g`, account, int(net[0])%64, int(net[1]))
	service, err := NewNetworkService(store, meshTestSigner{}, "https://issuer.example.test", NetworkOptions{})
	if err != nil {
		t.Fatal(err)
	}
	endpoint := account + "_m_0"
	stored := func() string {
		t.Helper()
		var value *string
		if err := store.SQL().QueryRowContext(ctx, `SELECT endpoints::text FROM paperboat.peer_network_identities WHERE user_id=$1 AND endpoint_id=$2`, account, endpoint).Scan(&value); err != nil {
			t.Fatal(err)
		}
		if value == nil {
			return "<none>"
		}
		return *value
	}
	topic := func() int64 {
		t.Helper()
		var generation int64
		if err := store.SQL().QueryRowContext(ctx, `SELECT coalesce(max(generation),0) FROM paperboat.account_topic_generations WHERE account_id=$1 AND topic='peer_network'`, account).Scan(&generation); err != nil {
			t.Fatal(err)
		}
		return generation
	}
	configure := func(op string, endpoints []string) NetworkConfigResult {
		t.Helper()
		result, err := service.Configuration(ctx, NetworkConfigRequest{OperationID: op + "_" + s, UserID: account, EndpointID: endpoint, Endpoints: endpoints})
		if err != nil {
			t.Fatalf("%s: %v", op, err)
		}
		return result
	}
	if result := configure("first", nil); !result.EndpointReports || stored() != "<none>" {
		t.Fatalf("before any report: endpoint_reports %v, stored %s", result.EndpointReports, stored())
	}
	before := topic()
	configure("report", []string{"203.0.113.7:41641", "10.0.0.5:41641", "203.0.113.7:41641"})
	if got := stored(); got != "{10.0.0.5:41641,203.0.113.7:41641}" {
		t.Fatalf("stored %s, want the normalized set", got)
	}
	configure("renewal", nil)
	if got := stored(); got != "{10.0.0.5:41641,203.0.113.7:41641}" {
		t.Fatalf("a renewal without endpoints changed the stored set to %s", got)
	}
	configure("moved", []string{"198.51.100.9:41641"})
	if got := stored(); got != "{198.51.100.9:41641}" {
		t.Fatalf("stored %s after the set changed", got)
	}
	if after := topic(); after != before {
		t.Fatalf("storing endpoints moved the peer_network topic %d -> %d; delivery is a later step", before, after)
	}
	if _, err := service.Configuration(ctx, NetworkConfigRequest{OperationID: "bad_" + s, UserID: account, EndpointID: endpoint, Endpoints: []string{"127.0.0.1:41641"}}); err == nil {
		t.Fatal("a loopback endpoint was accepted")
	}
}
