package operatorinfra

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
	"github.com/pinksaucepasta/paperboat/server/internal/nodeauth"
	"github.com/pinksaucepasta/paperboat/server/internal/operatoractions"
	"github.com/pinksaucepasta/paperboat/server/internal/selfhost"
)

func TestManagedRelayRuntimeLifecycleAndAudit(t *testing.T) {
	store := infraTestDB(t)
	ctx := context.Background()
	suffix := fmt.Sprint(time.Now().UnixNano())
	var actorBytes [16]byte
	if _, err := rand.Read(actorBytes[:]); err != nil {
		t.Fatal(err)
	}
	actor := "opr_" + hex.EncodeToString(actorBytes[:])
	if _, err := store.Pool().Exec(ctx, `INSERT INTO operator_accounts(id,email,password_hash,status,role) VALUES($1,$1||'@example.test','unused test login','active','admin')`, actor); err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("runtime-test", 4)
	service := NewService(store, key)
	region, err := service.MutateRegion(ctx, actor, "create-region-"+suffix, RegionMutation{Action: "create", ID: "runtime_" + suffix, DisplayName: "Runtime", Reason: "test"})
	if err != nil {
		t.Fatal(err)
	}
	nodeID := "relay_" + suffix
	cred := addManagedNode(t, store, key, nodeID, region.ID, region.ID, "zone-a", "relay.example.test", []string{"relay"}, []string{"derp_quic", "derp_wss"}, 16)
	defer func() {
		store.Pool().Exec(ctx, `DELETE FROM operator_infrastructure_operations WHERE actor_user_id=$1`, actor)
		store.Pool().Exec(ctx, `DELETE FROM selfhost_installations WHERE node_id=$1`, nodeID)
		store.Pool().Exec(ctx, `DELETE FROM control_tunnel_nodes WHERE id=$1`, nodeID)
		store.Pool().Exec(ctx, `DELETE FROM operator_regions WHERE id=$1`, region.ID)
		store.Pool().Exec(ctx, `DELETE FROM audit_events WHERE actor_user_id=$1`, actor)
		store.Pool().Exec(ctx, `DELETE FROM operator_accounts WHERE id=$1`, actor)
	}()
	assertRuntimeState := func(wantEpoch, wantState string, wantGeneration int64, wantReady bool) {
		t.Helper()
		var epoch, state string
		var generation int64
		var ready bool
		if err := store.Pool().QueryRow(ctx, `SELECT process_epoch,state,node_generation,ready FROM control_tunnel_nodes WHERE id=$1`, nodeID).Scan(&epoch, &state, &generation, &ready); err != nil {
			t.Fatal(err)
		}
		if epoch != wantEpoch || state != wantState || generation != wantGeneration || ready != wantReady {
			t.Fatalf("runtime state epoch=%q state=%s generation=%d ready=%t", epoch, state, generation, ready)
		}
	}
	assertRuntimeState("", "registered", 1, false)
	edge := controlplane.NewEdgeService(store, strings.Repeat("fleet", 10))
	edge.SetNodeCredentials(nodeauth.Composite{selfhost.New(store, key)})
	signer, err := mint.NewEphemeral(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	edge.SetSigner(signer)
	handler := edge.Handler()
	request := func(path, token string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		r := httptest.NewRequest("POST", path, strings.NewReader(string(raw)))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	start := func(token, epoch string, generation int64) *httptest.ResponseRecorder {
		return request("/v1/relay/nodes/start", token, map[string]any{"node_id": nodeID, "process_epoch": epoch, "expected_generation": generation})
	}
	observe := func(token, epoch string, generation int64, draining bool) *httptest.ResponseRecorder {
		return request("/v1/relay/nodes/observe", token, map[string]any{"node_id": nodeID, "process_epoch": epoch, "node_generation": generation, "ready": true, "draining": draining, "capacity_used": 1, "subjects": []any{}})
	}
	if w := request("/v1/relay/nodes/start", cred, map[string]any{"node_id": "other_node", "process_epoch": "run", "expected_generation": 1}); w.Code != 401 {
		t.Fatalf("wrong-node authority accepted: %d", w.Code)
	}
	if w := start(cred, "epoch-1", 1); w.Code != 200 {
		t.Fatalf("startup: %d %s", w.Code, w.Body.String())
	}
	if w := observe(cred, "epoch-1", 2, false); w.Code != 200 {
		t.Fatalf("runtime heartbeat: %d %s", w.Code, w.Body.String())
	}
	page, err := service.ListNodes(ctx, actor, region.ID, "", 1)
	if err != nil || len(page.Items) != 1 || !page.Items[0].Ready {
		t.Fatalf("observed readiness %+v %v", page, err)
	}
	rev := func() int64 {
		t.Helper()
		page, err := service.ListNodes(ctx, actor, region.ID, "", 1)
		if err != nil || len(page.Items) != 1 {
			t.Fatalf("read node %+v %v", page, err)
		}
		return page.Items[0].Revision
	}
	// Forced audit failure must roll back node state, revision and receipt.
	trigger := "infra_audit_" + suffix
	if _, err = store.Pool().Exec(ctx, `CREATE FUNCTION `+trigger+`() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.actor_user_id='`+actor+`' AND NEW.event_type='operator.infrastructure.node.drain' THEN RAISE EXCEPTION 'test audit rejection'; END IF; RETURN NEW; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Pool().Exec(ctx, `CREATE TRIGGER `+trigger+` BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION `+trigger+`() `); err != nil {
		t.Fatal(err)
	}
	defer func() {
		store.Pool().Exec(ctx, `DROP TRIGGER IF EXISTS `+trigger+` ON audit_events`)
		store.Pool().Exec(ctx, `DROP FUNCTION IF EXISTS `+trigger+`() `)
	}()
	drain := NodeMutation{Action: "drain", NodeID: nodeID, Revision: rev(), Reason: "test"}
	if out, err := service.MutateNode(ctx, actor, "audit-rollback-"+suffix, drain); err == nil || out.Node.ID != "" {
		t.Fatal("failed audit committed or disclosed")
	}
	var receipts int
	if err = store.Pool().QueryRow(ctx, `SELECT count(*) FROM operator_infrastructure_operations WHERE actor_user_id=$1 AND idempotency_key=$2`, actor, "audit-rollback-"+suffix).Scan(&receipts); err != nil || receipts != 0 {
		t.Fatal("failed audit retained receipt", err)
	}
	if _, err = store.Pool().Exec(ctx, `DROP TRIGGER `+trigger+` ON audit_events`); err != nil {
		t.Fatal(err)
	}
	_, err = service.MutateNode(ctx, actor, "drain-"+suffix, drain)
	if err != nil {
		t.Fatal(err)
	}
	if w := start(cred, "rogue-restart", 2); w.Code != 409 {
		t.Fatal("restart undid operator drain", w.Code)
	}
	if w := observe(cred, "epoch-1", 2, false); w.Code != 409 {
		t.Fatal("heartbeat undid operator drain", w.Code)
	}
	resumed, err := service.MutateNode(ctx, actor, "resume-"+suffix, NodeMutation{Action: "resume", NodeID: nodeID, Revision: rev(), Reason: "test"})
	if err != nil || resumed.Node.Ready {
		t.Fatal("resume should await observation", err)
	}
	if w := observe(cred, "epoch-1", 2, false); w.Code != 200 {
		t.Fatal("resume failed to restore runtime heartbeat", w.Code)
	}
	var runtimeVersion int64
	if err = store.Pool().QueryRow(ctx, `SELECT version FROM control_tunnel_nodes WHERE id=$1`, nodeID).Scan(&runtimeVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := operatoractions.NewService(store).Execute(ctx, actor, "competing-cas-"+suffix, "relay_drain", nodeID, "test", operatoractions.Input{Revision: runtimeVersion}); !errors.Is(err, operatoractions.ErrConflict) {
		t.Fatal("managed node admitted competing mutation authority", err)
	}
	disabled, err := service.MutateRegion(ctx, actor, "disable-"+suffix, RegionMutation{Action: "disable", ID: region.ID, Revision: region.Revision, Reason: "test"})
	if err != nil {
		t.Fatal(err)
	}
	var eligible bool
	if err = store.Pool().QueryRow(ctx, `SELECT node_pool_eligible($1,$2,'relay')`, nodeID, actor).Scan(&eligible); err != nil || eligible {
		t.Fatal("disabled region selected", err)
	}
	if _, err = service.MutateRegion(ctx, actor, "enable-"+suffix, RegionMutation{Action: "enable", ID: region.ID, Revision: disabled.Revision, Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	page, err = service.ListNodes(ctx, actor, region.ID, "", 1)
	if err != nil || page.Items[0].Ready {
		t.Fatal("reenable silently resumed node", err)
	}
	// A process shutdown withdraws readiness without becoming an operator drain.
	_, err = service.MutateNode(ctx, actor, "resume-after-enable-"+suffix, NodeMutation{Action: "resume", NodeID: nodeID, Revision: rev(), Reason: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if w := observe(cred, "epoch-1", 2, false); w.Code != 200 {
		t.Fatal("ready before process shutdown", w.Code)
	}
	for range 2 {
		if w := observe(cred, "epoch-1", 2, true); w.Code != 200 {
			t.Fatal("repeated process shutdown", w.Code)
		}
	}
	if w := observe(cred, "epoch-1", 2, false); w.Code != 409 {
		t.Fatal("shutdown process revived itself", w.Code)
	}
	if w := start(cred, "epoch-2", 2); w.Code != 200 {
		t.Fatal("fresh process cannot restart after shutdown", w.Code)
	}
	if w := observe(cred, "epoch-1", 2, false); w.Code != 409 {
		t.Fatal("old process survived replacement", w.Code)
	}
	if w := observe(cred, "epoch-2", 3, false); w.Code != 200 {
		t.Fatal("restarted process not ready", w.Code)
	}
	_, err = service.MutateNode(ctx, actor, "drain-restarted-"+suffix, NodeMutation{Action: "drain", NodeID: nodeID, Revision: rev(), Reason: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if w := observe(cred, "epoch-2", 3, true); w.Code != 200 {
		t.Fatal("operator drain observation", w.Code)
	}
	if w := start(cred, "epoch-3", 3); w.Code != 409 {
		t.Fatal("process shutdown cleared operator drain", w.Code)
	}
	_, err = service.MutateNode(ctx, actor, "resume-restarted-"+suffix, NodeMutation{Action: "resume", NodeID: nodeID, Revision: rev(), Reason: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if w := start(cred, "epoch-3", 3); w.Code != 200 {
		t.Fatal("operator resume failed to permit restart", w.Code)
	}
	page, err = service.ListNodes(ctx, actor, region.ID, "", 1)
	if err != nil || len(page.Items) != 1 {
		t.Fatal("read before retire", err)
	}
	retired, err := service.MutateNode(ctx, actor, "retire-"+suffix, NodeMutation{Action: "retire", NodeID: nodeID, Revision: rev(), Reason: "test"})
	if err != nil || retired.Node.State != "retired" {
		t.Fatal("retire", err)
	}
}
