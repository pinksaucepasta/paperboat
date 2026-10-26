package peersessions

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// endpointsFixture is one account with two personal machines (<account>_m_0, _m_1) that have identities and
// current certificates, and a network service whose signed claims land in *signed.
type endpointsFixture struct {
	ctx     context.Context
	store   *db.DB
	account string
	suffix  string
	service *NetworkService
	signed  *map[string]any
}

func newEndpointsFixture(t *testing.T) endpointsFixture {
	t.Helper()
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
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
	signed := map[string]any{}
	service, err := NewNetworkService(store, meshTestSigner{claims: &signed}, "https://issuer.example.test")
	if err != nil {
		t.Fatal(err)
	}
	return endpointsFixture{ctx: ctx, store: store, account: account, suffix: s, service: service, signed: &signed}
}

// topic is the account's peer_network topic generation.
func (f endpointsFixture) topic(t *testing.T) int64 {
	t.Helper()
	var generation int64
	if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT coalesce(max(generation),0) FROM paperboat.account_topic_generations WHERE account_id=$1 AND topic='peer_network'`, f.account).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	return generation
}

// configure requests machine n's configuration, reporting endpoints (nil: none sent).
func (f endpointsFixture) configure(t *testing.T, op string, n int, endpoints []string) NetworkConfigResult {
	t.Helper()
	result, err := f.service.Configuration(f.ctx, NetworkConfigRequest{OperationID: op + "_" + f.suffix, UserID: f.account, EndpointID: fmt.Sprintf("%s_m_%d", f.account, n), Endpoints: endpoints})
	if err != nil {
		t.Fatalf("%s: %v", op, err)
	}
	return result
}

// An engine reports its own candidate endpoints with its configuration request (PLAN 3b, N5, step 1): the answer
// says the server keeps them (endpoint_reports), the normalized set is stored on the identity, a request without
// endpoints keeps the stored set, a new set replaces it.
func TestReportedEndpointsAreStoredOnTheIdentityOnPostgres(t *testing.T) {
	f := newEndpointsFixture(t)
	store, ctx, account := f.store, f.ctx, f.account
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
	configure := func(op string, endpoints []string) NetworkConfigResult {
		t.Helper()
		return f.configure(t, op, 0, endpoints)
	}
	if result := configure("first", nil); !result.EndpointReports || stored() != "<none>" {
		t.Fatalf("before any report: endpoint_reports %v, stored %s", result.EndpointReports, stored())
	}
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
	if _, err := f.service.Configuration(ctx, NetworkConfigRequest{OperationID: "bad_" + f.suffix, UserID: account, EndpointID: endpoint, Endpoints: []string{"127.0.0.1:41641"}}); err == nil {
		t.Fatal("a loopback endpoint was accepted")
	}
}

// peerEndpoints is what the last signed configuration gives each peer, and whether the field appears at all.
func (f endpointsFixture) peerEndpoints(t *testing.T) (map[string][]string, bool) {
	t.Helper()
	raw, err := json.Marshal(*f.signed)
	if err != nil {
		t.Fatal(err)
	}
	var claims struct {
		Peers []struct {
			Identity struct {
				EndpointID string `json:"endpoint_id"`
			} `json:"identity"`
			Endpoints []string `json:"endpoints"`
		} `json:"peers"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, p := range claims.Peers {
		out[p.Identity.EndpointID] = p.Endpoints
	}
	return out, strings.Contains(string(raw), `"endpoints"`)
}

// A peer's reported endpoints reach the other engines on its peer entry (PLAN 3b, N5, step 2), and only those
// that declared they accept them (an endpoints field, even empty, on any of their requests): an engine that never
// sent one gets a configuration without the field, as before, because it refuses unknown signed fields. A change
// of the set publishes peer_network at most once a minute per identity, the latest set winning: a NAT flapping
// between mappings inside the minute moves no topic, and the set it settles on is published by the
// server once the minute has passed, with one topic move.
func TestPeerEndpointsReachOnlyCapableRecipientsCoalescedOnPostgres(t *testing.T) {
	f := newEndpointsFixture(t)
	peer := f.account + "_m_1"
	start := time.Now().UTC()
	clock := start
	f.service.now = func() time.Time { return clock }

	f.configure(t, "legacy", 0, nil)
	if _, present := f.peerEndpoints(t); present {
		t.Fatal("a configuration carried endpoints before anyone reported any")
	}
	before := f.topic(t)
	f.configure(t, "first_report", 1, []string{"203.0.113.7:41641", "10.0.0.5:41641"})
	if after := f.topic(t); after != before+1 {
		t.Fatalf("the first reported set moved the peer_network topic %d -> %d, want one move", before, after)
	}
	f.configure(t, "legacy_again", 0, nil)
	if _, present := f.peerEndpoints(t); present {
		t.Fatal("a recipient that never sent endpoints was given peers' endpoints")
	}
	f.configure(t, "declares", 0, []string{})
	got, _ := f.peerEndpoints(t)
	if want := []string{"10.0.0.5:41641", "203.0.113.7:41641"}; fmt.Sprint(got[peer]) != fmt.Sprint(want) {
		t.Fatalf("capable recipient got %v for %s, want %v", got[peer], peer, want)
	}
	f.configure(t, "renewal", 0, nil)
	if got, _ := f.peerEndpoints(t); len(got[peer]) != 2 {
		t.Fatalf("a renewal without the field unmarked the recipient: %v", got[peer])
	}

	// The NAT flaps inside the minute: every set is stored, none is published.
	flapped := f.topic(t)
	for i := 0; i < 20; i++ {
		clock = start.Add(time.Duration(i+1) * 2 * time.Second)
		f.configure(t, fmt.Sprintf("flap_%d", i), 1, []string{fmt.Sprintf("198.51.100.%d:41641", 1+i%3)})
	}
	if after := f.topic(t); after != flapped {
		t.Fatalf("a flapping NAT moved the peer_network topic %d times inside the minute", after-flapped)
	}
	f.configure(t, "inside", 0, nil)
	if got, _ := f.peerEndpoints(t); len(got[peer]) != 2 {
		t.Fatalf("a set inside the minute was delivered: %v", got[peer])
	}
	due := func() bool {
		t.Helper()
		var held bool
		if err := f.store.SQL().QueryRowContext(f.ctx, `SELECT endpoints_due_at IS NOT NULL FROM paperboat.peer_network_identities WHERE user_id=$1 AND endpoint_id=$2`, f.account, peer).Scan(&held); err != nil {
			t.Fatal(err)
		}
		return held
	}
	publish := func() {
		t.Helper()
		if _, err := f.service.PublishDueEndpoints(f.ctx); err != nil {
			t.Fatal(err)
		}
	}
	publish()
	if !due() || f.topic(t) != flapped {
		t.Fatalf("inside the minute: held back %v, topic moves %d", due(), f.topic(t)-flapped)
	}
	// The minute passes: the server publishes the latest set once, without waiting for the peer's next request,
	// and the peer's next requests move nothing more.
	clock = start.Add(endpointPublishInterval + time.Second)
	publish()
	publish()
	if due() {
		t.Fatal("the published set is still held back")
	}
	f.configure(t, "after_minute", 1, nil)
	if after := f.topic(t); after != flapped+1 {
		t.Fatalf("after the minute the topic moved %d times, want 1", after-flapped)
	}
	f.configure(t, "settled", 0, nil)
	if got, _ := f.peerEndpoints(t); fmt.Sprint(got[peer]) != "[198.51.100.2:41641]" {
		t.Fatalf("delivered %v after the minute, want the latest set [198.51.100.2:41641]", got[peer])
	}
	// A set held back and then reported back to the delivered one is no longer due.
	clock = clock.Add(time.Second)
	f.configure(t, "away", 1, []string{"192.0.2.1:41641"})
	f.configure(t, "back", 1, []string{"198.51.100.2:41641"})
	clock = clock.Add(endpointPublishInterval)
	publish()
	if due() || f.topic(t) != flapped+1 {
		t.Fatalf("a set that went back to the delivered one: held back %v, topic moves %d", due(), f.topic(t)-flapped)
	}
	// The set going back to what is delivered moves nothing; an empty report clears it with one move.
	clock = clock.Add(endpointPublishInterval + time.Second)
	f.configure(t, "same", 1, []string{"198.51.100.2:41641"})
	if after := f.topic(t); after != flapped+1 {
		t.Fatal("reporting the delivered set again moved the topic")
	}
	f.configure(t, "vanished", 1, []string{})
	f.configure(t, "cleared", 0, nil)
	if got, _ := f.peerEndpoints(t); got[peer] != nil || f.topic(t) != flapped+2 {
		t.Fatalf("after the candidates vanished: delivered %v, topic moves %d", got[peer], f.topic(t)-flapped)
	}
}
