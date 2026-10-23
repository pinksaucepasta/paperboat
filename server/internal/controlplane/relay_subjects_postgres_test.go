package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/telemetry"
)

// relaySubjectsNode is a relay node with one account whose endpoints the observation tests revoke or keep.
type relaySubjectsNode struct {
	t          *testing.T
	store      *db.DB
	service    *EdgeService
	id         string
	epoch      string
	account    string
	generation int64
}

var relaySubjectsCounter atomic.Int64

func newRelaySubjectsNode(t *testing.T, name string) *relaySubjectsNode {
	store := openControlPlaneTestDB(t)
	ctx := t.Context()
	x := fmt.Sprint(time.Now().UnixNano())
	n := &relaySubjectsNode{t: t, store: store, service: NewEdgeService(store, "relay-subjects-test-credential-0123456789"), id: name + "_node_" + x, epoch: name + "_epoch_" + x, account: name + "_acct_" + x, generation: 1}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM peer_network_identities WHERE user_id=$1`, n.account)
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM peer_endpoint_certificates WHERE user_id=$1`, n.account)
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM control_tunnel_nodes WHERE id=$1`, n.id)
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM users WHERE id=$1`, n.account)
	})
	n.exec(ctx, `INSERT INTO users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@example.test','active')`, n.account)
	now := time.Now().UTC()
	n.exec(ctx, `INSERT INTO control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,endpoint_host,endpoint_tcp_port,endpoint_quic_port,state,ready,last_heartbeat_at,node_generation,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at) VALUES($1,'default','v1',$2,'relay.example.test',443,444,'ready',true,$3,1,'hel','hel-a',ARRAY['relay'],ARRAY['derp_quic'],8192,0,$3,$4)`, n.id, n.epoch, now, now.Add(time.Hour))
	return n
}

func (n *relaySubjectsNode) exec(ctx context.Context, query string, args ...any) {
	n.t.Helper()
	if _, err := n.store.SQL().ExecContext(ctx, query, args...); err != nil {
		n.t.Fatal(err)
	}
}

func (n *relaySubjectsNode) count(query string, args ...any) int {
	n.t.Helper()
	var c int
	if err := n.store.SQL().QueryRowContext(n.t.Context(), query, args...).Scan(&c); err != nil {
		n.t.Fatal(err)
	}
	return c
}

// identity registers an eligible CLI endpoint: floor is its relay revocation generation, config its configuration generation.
func (n *relaySubjectsNode) identity(endpoint string, floor, config int64) {
	n.t.Helper()
	fingerprint := sha256.Sum256([]byte(endpoint))
	index := relaySubjectsCounter.Add(1)
	n.exec(n.t.Context(), `INSERT INTO paperboat.peer_endpoint_certificates(fingerprint,user_id,endpoint_id,role,generation,serial,certificate,quic_public_key,issued_at,expires_at,key_id,signer_public_key) VALUES($1,$2,$3,'cli',1,$4,$5,$6,now()-interval '1 minute',now()+interval '1 day','key',$6)`, fingerprint[:], n.account, endpoint, index, make([]byte, 200), make([]byte, 32))
	n.exec(n.t.Context(), `INSERT INTO paperboat.peer_network_identities(user_id,endpoint_id,role,machine_id,endpoint_generation,machine_generation,key_generation,wireguard_public_key,quic_certificate_fingerprint,virtual_address,relay_revocation_generation,config_generation,created_at,updated_at) VALUES($1,$2,'cli',NULL,1,0,1,$3,$3,$4::inet,$5,$6,now(),now())`, n.account, endpoint, fingerprint[:], fmt.Sprintf("fd7a:115c:a1f0::%x", index), floor, config)
}

// members are the endpoints as a digest relay sends them: membership only.
func (n *relaySubjectsNode) members(endpoints ...string) []relaySubject {
	out := make([]relaySubject, 0, len(endpoints))
	for _, endpoint := range endpoints {
		out = append(out, relaySubject{AccountID: n.account, EndpointID: endpoint})
	}
	return out
}

// generations are the endpoints as a relay without subjects_digest sends them: each with its grant generation 1.
func (n *relaySubjectsNode) generations(endpoints ...string) []relaySubject {
	out := n.members(endpoints...)
	for i := range out {
		out[i].Generation = 1
	}
	return out
}

// observe sends a full observation (subjects, digest) or a digest-only one (nil subjects) for the node's current generation.
func (n *relaySubjectsNode) observe(subjects []relaySubject, digest string) (relayNodeLeaseDocument, error) {
	return n.service.ObserveRelayNode(n.t.Context(), relayNodeObservation{NodeID: n.id, Generation: n.generation, ProcessEpoch: n.epoch, Ready: true, Subjects: subjects, SubjectsDigest: digest})
}

func (n *relaySubjectsNode) mustObserve(subjects []relaySubject, digest string) relayNodeLeaseDocument {
	n.t.Helper()
	out, err := n.observe(subjects, digest)
	if err != nil {
		n.t.Fatalf("observation failed: %v", err)
	}
	return out
}

func relaySubjectsDigestOf(t *testing.T, subjects []relaySubject) string {
	t.Helper()
	set, err := newRelaySubjectSet(subjects)
	if err != nil {
		t.Fatal(err)
	}
	return set.digest
}

// floors maps each revoked endpoint to its min_generation (digest relays) or generation (older relays).
func floors(list []relayRevocation) map[string]int64 {
	out := map[string]int64{}
	for _, r := range list {
		out[r.EndpointID] = max(r.MinGeneration, r.Generation)
	}
	return out
}

// A stored membership answers digest-only observations with the same revocations as the full list, and those
// revocations follow the identities' live floors. A member with no identity is cut at the largest generation.
func TestRelaySubjectsStoredListServesDigestOnlyObservationPostgres(t *testing.T) {
	n := newRelaySubjectsNode(t, "relay_store")
	n.identity("ep_revoked", 5, 5)
	n.identity("ep_kept", 1, 1)
	list := n.members("ep_kept", "ep_missing", "ep_revoked")
	digest := relaySubjectsDigestOf(t, list)

	full := n.mustObserve(list, digest)
	if full.SubjectsRequired {
		t.Fatal("a full list with its digest asked for the list")
	}
	want := map[string]int64{"ep_revoked": 5, "ep_missing": math.MaxInt64}
	if got := floors(full.Revocations); len(got) != len(want) || got["ep_revoked"] != 5 || got["ep_missing"] != math.MaxInt64 {
		t.Fatalf("full list revocations = %v, want %v", got, want)
	}
	digestOnly := n.mustObserve(nil, digest)
	if digestOnly.SubjectsRequired || len(digestOnly.Revocations) != len(full.Revocations) {
		t.Fatalf("digest-only observation = %+v, want the full list's revocations", digestOnly)
	}
	for i := range full.Revocations {
		if full.Revocations[i] != digestOnly.Revocations[i] {
			t.Fatalf("digest-only revocation %d = %+v, want %+v", i, digestOnly.Revocations[i], full.Revocations[i])
		}
	}

	// The stored membership names endpoints; floors are read live on every observation.
	n.exec(t.Context(), `UPDATE paperboat.peer_network_identities SET relay_revocation_generation=7 WHERE user_id=$1 AND endpoint_id='ep_kept'`, n.account)
	live := floors(n.mustObserve(nil, digest).Revocations)
	if live["ep_kept"] != 7 || live["ep_revoked"] != 5 || live["ep_missing"] != math.MaxInt64 {
		t.Fatalf("digest-only revocations after a floor change = %v", live)
	}
}

// A digest relay sees min_generation and no generation in each revocation entry; an older relay sees generation only.
func TestRelaySubjectsLeaseShapesPostgres(t *testing.T) {
	n := newRelaySubjectsNode(t, "relay_shape")
	n.identity("ep_revoked", 5, 5)
	digestLease, err := n.observe(n.members("ep_revoked"), relaySubjectsDigestOf(t, n.members("ep_revoked")))
	if err != nil {
		t.Fatal(err)
	}
	digestJSON, _ := json.Marshal(digestLease)
	if !strings.Contains(string(digestJSON), `"revocations":[{"account_id":"`+n.account+`","endpoint_id":"ep_revoked","min_generation":5}]`) {
		t.Fatalf("digest relay lease = %s, want one min_generation entry", digestJSON)
	}
	olderLease, err := n.observe(n.generations("ep_revoked"), "")
	if err != nil {
		t.Fatal(err)
	}
	olderJSON, _ := json.Marshal(olderLease)
	if !strings.Contains(string(olderJSON), `"revocations":[{"account_id":"`+n.account+`","endpoint_id":"ep_revoked","generation":5}]`) || strings.Contains(string(olderJSON), "subjects_required") {
		t.Fatalf("older relay lease = %s, want one generation entry and no subjects_required", olderJSON)
	}
	if got := n.count(`SELECT count(*) FROM paperboat.relay_subject_sets WHERE node_id=$1`, n.id); got != 1 {
		t.Fatalf("stored lists = %d, want the digest relay's one", got)
	}
}

// Grant generations change on every peer-network refresh while membership stays: the stored list keeps its digest
// and is not rewritten, and digest-only observations keep using it.
func TestRelaySubjectsSteadyMembershipKeepsStoredSetPostgres(t *testing.T) {
	n := newRelaySubjectsNode(t, "relay_steady")
	n.identity("ep_a", 0, 1)
	n.identity("ep_b", 0, 1)
	list := n.members("ep_a", "ep_b")
	digest := relaySubjectsDigestOf(t, list)
	n.mustObserve(list, digest)
	var storedAt time.Time
	if err := n.store.SQL().QueryRowContext(t.Context(), `SELECT stored_at FROM paperboat.relay_subject_sets WHERE node_id=$1`, n.id).Scan(&storedAt); err != nil {
		t.Fatal(err)
	}

	n.exec(t.Context(), `UPDATE paperboat.peer_network_identities SET config_generation=config_generation+3 WHERE user_id=$1`, n.account)
	time.Sleep(10 * time.Millisecond)
	again := n.mustObserve(list, digest)
	if again.SubjectsRequired {
		t.Fatal("unchanged membership asked for the list")
	}
	if digestOnly := n.mustObserve(nil, digest); digestOnly.SubjectsRequired {
		t.Fatal("digest-only observation after a generation refresh asked for the list")
	}
	var after time.Time
	if err := n.store.SQL().QueryRowContext(t.Context(), `SELECT stored_at FROM paperboat.relay_subject_sets WHERE node_id=$1`, n.id).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !after.Equal(storedAt) {
		t.Fatalf("stored set was rewritten at %v (stored %v)", after, storedAt)
	}
}

// Without a stored list that matches the digest, the server asks for the full list and computes nothing.
func TestRelaySubjectsUnknownOrChangedDigestAsksForListPostgres(t *testing.T) {
	n := newRelaySubjectsNode(t, "relay_ask")
	n.identity("ep_revoked", 5, 5)
	list := n.members("ep_revoked", "ep_missing")
	digest := relaySubjectsDigestOf(t, list)

	asked := n.mustObserve(nil, digest)
	if !asked.SubjectsRequired || len(asked.Revocations) != 0 {
		t.Fatalf("unknown digest answered %+v, want subjects_required with no revocations", asked)
	}
	if got := n.count(`SELECT count(*) FROM paperboat.relay_subject_sets WHERE node_id=$1`, n.id); got != 0 {
		t.Fatalf("a digest-only observation stored %d lists", got)
	}

	n.mustObserve(list, digest)
	other := relaySubjectsDigestOf(t, n.members("ep_other"))
	changed := n.mustObserve(nil, other)
	if !changed.SubjectsRequired || len(changed.Revocations) != 0 {
		t.Fatalf("changed digest answered %+v, want subjects_required with no revocations", changed)
	}
	if got := n.count(`SELECT count(*) FROM paperboat.relay_subject_sets WHERE node_id=$1 AND digest=$2`, n.id, digest); got != 1 {
		t.Fatalf("the stored list was replaced by a digest-only observation (%d matches)", got)
	}

	// A full list that does not hash to its digest is an invalid request and changes nothing.
	if _, err := n.observe(n.members("ep_other"), digest); !errors.Is(err, ErrInvalidUsageReport) {
		t.Fatalf("mismatched digest error = %v, want invalid usage", err)
	}
	if got := n.count(`SELECT count(*) FROM paperboat.relay_subject_sets WHERE node_id=$1 AND digest=$2`, n.id, digest); got != 1 {
		t.Fatalf("a mismatched observation changed the stored list (%d matches)", got)
	}
}

// A digest relay that sends grant generations is refused: its membership carries none.
func TestRelaySubjectsDigestRelayRejectsGenerationsPostgres(t *testing.T) {
	n := newRelaySubjectsNode(t, "relay_gen_reject")
	list := n.generations("ep_a")
	if _, err := n.observe(list, relaySubjectsDigestOf(t, n.members("ep_a"))); !errors.Is(err, ErrInvalidUsageReport) {
		t.Fatalf("digest relay with generations error = %v, want invalid usage", err)
	}
}

// Storing a list for a new generation or epoch drops the lists of the earlier ones.
func TestRelaySubjectsStoredListFollowsNodeGenerationPostgres(t *testing.T) {
	n := newRelaySubjectsNode(t, "relay_gen")
	n.identity("ep_revoked", 5, 5)
	list := n.members("ep_revoked")
	digest := relaySubjectsDigestOf(t, list)
	n.mustObserve(list, digest)

	n.exec(t.Context(), `UPDATE paperboat.control_tunnel_nodes SET node_generation=2,process_epoch=$2 WHERE id=$1`, n.id, n.epoch+"_2")
	n.generation, n.epoch = 2, n.epoch+"_2"
	if got := n.mustObserve(nil, digest); !got.SubjectsRequired {
		t.Fatalf("a new generation used the previous generation's list: %+v", got)
	}
	n.mustObserve(list, digest)
	if got := n.count(`SELECT count(*) FROM paperboat.relay_subject_sets WHERE node_id=$1 AND process_epoch=$2 AND node_generation=2`, n.id, n.epoch); got != 1 {
		t.Fatalf("the new generation's list is not stored (%d rows)", got)
	}
	if got := n.count(`SELECT count(*) FROM paperboat.relay_subject_sets WHERE node_id=$1`, n.id); got != 1 {
		t.Fatalf("the node keeps %d lists, want one", got)
	}
}

// An account the node's pool no longer admits is cut above every grant minted for it, even with floor 0.
func TestRelaySubjectsIneligibleAccountIsCutAboveItsGrantsPostgres(t *testing.T) {
	n := newRelaySubjectsNode(t, "relay_pool")
	n.identity("ep_a", 0, 4)
	// A grant can carry the relay revocation generation, which may exceed the configuration generation
	// (the relay lifecycle test's grant was at 3 with configuration generation 1): the cut is above both.
	n.identity("ep_b", 7, 4)
	n.exec(t.Context(), `UPDATE control_tunnel_nodes SET allowed_account_ids=ARRAY['someone_else'] WHERE id=$1`, n.id)
	got := floors(n.mustObserve(n.members("ep_a", "ep_b"), relaySubjectsDigestOf(t, n.members("ep_a", "ep_b"))).Revocations)
	if got["ep_a"] != 5 || got["ep_b"] != 8 {
		t.Fatalf("ineligible endpoints cut at %d and %d, want 5 and 8 (the higher of configuration and revocation generation, + 1)", got["ep_a"], got["ep_b"])
	}
}

// A full list of 4,096 members is revoked by one query, with the result the per-member rule gives: a member with
// no identity is cut at the largest generation, a floor above the first generation at the floor, and a floor of
// one (or none) is not listed.
func TestRelaySubjectsFullListOf4096RevokesInOneQueryPostgres(t *testing.T) {
	n := newRelaySubjectsNode(t, "relay_4096")
	const total = 4096
	ctx := t.Context()
	// Endpoints 1..4096: i%3==0 has no identity, i%3==1 has floor 5, i%3==2 has floor 1 (kept).
	n.exec(ctx, `INSERT INTO paperboat.peer_endpoint_certificates(fingerprint,user_id,endpoint_id,role,generation,serial,certificate,quic_public_key,issued_at,expires_at,key_id,signer_public_key)
SELECT sha256(convert_to('endpoint_'||lpad(i::text,4,'0'),'UTF8')),$1,'endpoint_'||lpad(i::text,4,'0'),'cli',1,i,decode(repeat('00',200),'hex'),decode(repeat('00',32),'hex'),now()-interval '1 minute',now()+interval '1 day','key',decode(repeat('00',32),'hex')
FROM generate_series(1,$2::int) i WHERE i%3<>0`, n.account, total)
	n.exec(ctx, `INSERT INTO paperboat.peer_network_identities(user_id,endpoint_id,role,machine_id,endpoint_generation,machine_generation,key_generation,wireguard_public_key,quic_certificate_fingerprint,virtual_address,relay_revocation_generation,created_at,updated_at)
SELECT $1,'endpoint_'||lpad(i::text,4,'0'),'cli',NULL,1,0,1,sha256(convert_to('endpoint_'||lpad(i::text,4,'0'),'UTF8')),sha256(convert_to('endpoint_'||lpad(i::text,4,'0'),'UTF8')),('fd7a:115c:a1f1::'||to_hex(i))::inet,CASE i%3 WHEN 1 THEN 5 ELSE 1 END,now(),now()
FROM generate_series(1,$2::int) i WHERE i%3<>0`, n.account, total)

	list := make([]relaySubject, 0, total)
	want := map[string]int64{}
	for i := 1; i <= total; i++ {
		endpoint := fmt.Sprintf("endpoint_%04d", i)
		list = append(list, relaySubject{AccountID: n.account, EndpointID: endpoint})
		switch i % 3 {
		case 0:
			want[endpoint] = math.MaxInt64
		case 1:
			want[endpoint] = 5
		}
	}
	set, err := newRelaySubjectSet(list)
	if err != nil {
		t.Fatal(err)
	}
	revoked, queries := relaySetRevocations(t, n, set)
	if queries != 1 {
		t.Fatalf("revocations took %d queries, want one set query", queries)
	}
	got := floors(revoked)
	if len(got) != len(want) {
		t.Fatalf("revoked %d members, want %d", len(got), len(want))
	}
	for endpoint, floor := range want {
		if got[endpoint] != floor {
			t.Fatalf("%s cut at %d, want %d", endpoint, got[endpoint], floor)
		}
	}
	// The same list through the observe path, digest-only afterwards, gives the same revocations.
	if digestOnly := floors(n.mustObserve(list, set.digest).Revocations); len(digestOnly) != len(want) {
		t.Fatalf("full observation revoked %d members, want %d", len(digestOnly), len(want))
	}
}

// relaySetRevocations runs one revocation pass in its own transaction, which ends before any observation
// needs a connection of its own, and reports how many statements the pass issued.
func relaySetRevocations(t *testing.T, n *relaySubjectsNode, set relaySubjectSet) ([]relayRevocation, int) {
	t.Helper()
	ctx := t.Context()
	tx, err := n.store.SQL().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	counter := &countingQueryer{tx: tx}
	revoked, err := relayMinGenerations(ctx, counter, n.id, set)
	if err != nil {
		t.Fatal(err)
	}
	return revoked, counter.queries
}

// countingQueryer counts the statements a revocation pass issues.
type countingQueryer struct {
	tx      *sql.Tx
	queries int
}

func (c *countingQueryer) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	c.queries++
	return c.tx.QueryContext(ctx, query, args...)
}

// A full membership of 4,096 endpoints with long identifiers is larger than the general edge document limit;
// the observe route must still accept it.
func TestRelaySubjectsObservationBodyCarriesFullListPostgres(t *testing.T) {
	n := newRelaySubjectsNode(t, "relay_body")
	list := make([]relaySubject, 0, 4096)
	for i := range 4096 {
		endpoint := strings.Repeat("e", 230) + fmt.Sprintf("%04d", i)
		list = append(list, relaySubject{AccountID: n.account, EndpointID: endpoint})
	}
	body, err := json.Marshal(relayNodeObservation{NodeID: n.id, Generation: 1, ProcessEpoch: n.epoch, Ready: true, Subjects: list, SubjectsDigest: relaySubjectsDigestOf(t, list)})
	if err != nil {
		t.Fatal(err)
	}
	if len(body) <= maxEdgeDocument {
		t.Fatalf("test body is %d bytes, want more than the general limit %d", len(body), maxEdgeDocument)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/relay/nodes/observe", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	n.service.handleRelayObserve(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("observe status = %d: %s", rec.Code, rec.Body.String())
	}
	var lease relayNodeLeaseDocument
	if err := json.Unmarshal(rec.Body.Bytes(), &lease); err != nil {
		t.Fatal(err)
	}
	if len(lease.Revocations) != len(list) {
		t.Fatalf("revocations = %d, want %d (no identity rows)", len(lease.Revocations), len(list))
	}
}

// Observations are counted by the shape they carried: a full list (including older relays), a digest answered from
// the stored list, or a digest that asked for the list.
func TestRelaySubjectsObservationsCountedByShapePostgres(t *testing.T) {
	n := newRelaySubjectsNode(t, "relay_count")
	metrics := telemetry.NewMetrics()
	n.service.SetRelayMetrics(metrics)
	list := n.members("ep_a")
	digest := relaySubjectsDigestOf(t, list)
	n.mustObserve(list, digest)
	n.mustObserve(nil, digest)
	n.mustObserve(nil, relaySubjectsDigestOf(t, n.members("ep_other")))
	n.mustObserve(n.generations("ep_a"), "")

	want := map[string]uint64{"full": 2, "digest_only": 1, "required": 1}
	got := map[string]uint64{}
	for _, sample := range metrics.Snapshot() {
		if sample.Name != telemetry.MetricRelaySubjectObservations {
			continue
		}
		for _, label := range sample.Labels {
			if label.Name == "shape" {
				got[label.Value] = sample.Value
			}
		}
	}
	for shape, value := range want {
		if got[shape] != value {
			t.Fatalf("observations counted = %v, want %v", got, want)
		}
	}
}
