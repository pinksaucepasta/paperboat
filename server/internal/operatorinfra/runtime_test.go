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
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat/server/internal/nodeauth"
	"github.com/pinksaucepasta/paperboat/server/internal/operatoractions"
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
	service := NewService(store, strings.Repeat("runtime-test", 4))
	region, err := service.MutateRegion(ctx, actor, "create-region-"+suffix, RegionMutation{Action: "create", ID: "runtime_" + suffix, DisplayName: "Runtime", Reason: "test"})
	if err != nil {
		t.Fatal(err)
	}
	input := NodeMutation{Action: "enroll", NodeID: "relay_" + suffix, RegionID: region.ID, EdgePool: region.ID, FailureDomain: "zone-a", EndpointHost: "relay.example.test", EndpointTCPPort: 443, EndpointQUICPort: 444, Roles: []string{"relay"}, Transports: []string{"derp_quic", "derp_wss"}, CapacityLimit: 16, Reason: "test"}
	enrolled, err := service.MutateNode(ctx, actor, "enroll-"+suffix, input)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		store.Pool().Exec(ctx, `DELETE FROM operator_infrastructure_operations WHERE actor_user_id=$1`, actor)
		store.Pool().Exec(ctx, `DELETE FROM managed_node_authority WHERE node_id=$1`, input.NodeID)
		store.Pool().Exec(ctx, `DELETE FROM control_tunnel_nodes WHERE id=$1`, input.NodeID)
		store.Pool().Exec(ctx, `DELETE FROM operator_regions WHERE id=$1`, region.ID)
		store.Pool().Exec(ctx, `DELETE FROM audit_events WHERE actor_user_id=$1`, actor)
		store.Pool().Exec(ctx, `DELETE FROM operator_accounts WHERE id=$1`, actor)
	}()
	assertRuntimeState := func(wantEpoch, wantState string, wantGeneration int64, wantReady bool) {
		t.Helper()
		var epoch, state string
		var generation int64
		var ready bool
		if err := store.Pool().QueryRow(ctx, `SELECT process_epoch,state,node_generation,ready FROM control_tunnel_nodes WHERE id=$1`, input.NodeID).Scan(&epoch, &state, &generation, &ready); err != nil {
			t.Fatal(err)
		}
		if epoch != wantEpoch || state != wantState || generation != wantGeneration || ready != wantReady {
			t.Fatalf("runtime state epoch=%q state=%s generation=%d ready=%t", epoch, state, generation, ready)
		}
	}
	assertRuntimeState("", "registered", 1, false)
	edge := controlplane.NewEdgeService(store, strings.Repeat("fleet", 10))
	edge.SetNodeCredentials(nodeauth.Composite{service})
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
		return request("/v1/relay/nodes/start", token, map[string]any{"node_id": input.NodeID, "process_epoch": epoch, "expected_generation": generation})
	}
	observe := func(token, epoch string, generation int64, draining bool) *httptest.ResponseRecorder {
		return request("/v1/relay/nodes/observe", token, map[string]any{"node_id": input.NodeID, "process_epoch": epoch, "node_generation": generation, "ready": true, "draining": draining, "capacity_used": 1, "subjects": []any{}})
	}
	if w := request("/v1/relay/nodes/start", enrolled.RuntimeCredential, map[string]any{"node_id": "other_node", "process_epoch": "run", "expected_generation": 1}); w.Code != 401 {
		t.Fatalf("wrong-node authority accepted: %d", w.Code)
	}
	if w := start(enrolled.RuntimeCredential, "epoch-1", enrolled.Node.NodeGeneration); w.Code != 200 {
		t.Fatalf("enrolled startup: %d %s", w.Code, w.Body.String())
	}
	if w := observe(enrolled.RuntimeCredential, "epoch-1", 2, false); w.Code != 200 {
		t.Fatalf("runtime heartbeat: %d %s", w.Code, w.Body.String())
	}
	page, err := service.ListNodes(ctx, actor, region.ID, "", 1)
	if err != nil || len(page.Items) != 1 || !page.Items[0].Ready {
		t.Fatalf("observed readiness %+v %v", page, err)
	}
	// A configuration edit fences old runtime generation and observations.
	input.Action = "update"
	input.Revision = page.Items[0].Revision
	input.EndpointHost = "replacement.example.test"
	changed, err := service.MutateNode(ctx, actor, "config-update-"+suffix, input)
	if err != nil || changed.Node.Ready || changed.Node.NodeGeneration != 3 {
		t.Fatalf("configuration fence %+v %v", changed.Node, err)
	}
	assertRuntimeState("", "registered", 3, false)
	if w := observe(enrolled.RuntimeCredential, "epoch-1", 2, false); w.Code != 409 {
		t.Fatalf("obsolete process revived changed configuration: %d", w.Code)
	}
	if w := start(enrolled.RuntimeCredential, "epoch-2", changed.Node.NodeGeneration); w.Code != 200 {
		t.Fatalf("replacement startup %d %s", w.Code, w.Body.String())
	}
	if w := observe(enrolled.RuntimeCredential, "epoch-2", 4, false); w.Code != 200 {
		t.Fatalf("replacement ready %d", w.Code)
	}
	assertRuntimeState("epoch-2", "ready", 4, true)
	// One actor/key has one immutable result even when rotations race.
	rotate := NodeMutation{Action: "rotate", NodeID: input.NodeID, Revision: changed.Node.Revision, Reason: "test"}
	type result struct {
		value NodeMutationResult
		err   error
	}
	ch := make(chan result, 3)
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := service.MutateNode(ctx, actor, "rotate-race-"+suffix, rotate)
			ch <- result{out, err}
		}()
	}
	wg.Wait()
	close(ch)
	var rotated NodeMutationResult
	for out := range ch {
		if out.err != nil {
			t.Fatal(out.err)
		}
		if rotated.RuntimeCredential != "" && rotated.RuntimeCredential != out.value.RuntimeCredential {
			t.Fatal("concurrent credentials differ")
		}
		rotated = out.value
	}
	if w := observe(enrolled.RuntimeCredential, "epoch-2", 4, false); w.Code != 401 {
		t.Fatal("old runtime credential survived rotation")
	}
	if w := observe(rotated.RuntimeCredential, "epoch-2", 4, false); w.Code != 200 {
		t.Fatal("new runtime credential rejected", w.Code)
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
	drain := NodeMutation{Action: "drain", NodeID: input.NodeID, Revision: rotated.Node.Revision, Reason: "test"}
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
	drained, err := service.MutateNode(ctx, actor, "drain-"+suffix, drain)
	if err != nil {
		t.Fatal(err)
	}
	if w := start(rotated.RuntimeCredential, "rogue-restart", 4); w.Code != 409 {
		t.Fatal("restart undid operator drain", w.Code)
	}
	if w := observe(rotated.RuntimeCredential, "epoch-2", 4, false); w.Code != 409 {
		t.Fatal("heartbeat undid operator drain", w.Code)
	}
	resumed, err := service.MutateNode(ctx, actor, "resume-"+suffix, NodeMutation{Action: "resume", NodeID: input.NodeID, Revision: drained.Node.Revision, Reason: "test"})
	if err != nil || resumed.Node.Ready {
		t.Fatal("resume should await observation", err)
	}
	if w := observe(rotated.RuntimeCredential, "epoch-2", 4, false); w.Code != 200 {
		t.Fatal("resume failed to restore runtime heartbeat", w.Code)
	}
	var runtimeVersion int64
	if err = store.Pool().QueryRow(ctx, `SELECT version FROM control_tunnel_nodes WHERE id=$1`, input.NodeID).Scan(&runtimeVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := operatoractions.NewService(store).Execute(ctx, actor, "competing-cas-"+suffix, "relay_drain", input.NodeID, "test", operatoractions.Input{Revision: runtimeVersion}); !errors.Is(err, operatoractions.ErrConflict) {
		t.Fatal("managed node admitted competing mutation authority", err)
	}
	disabled, err := service.MutateRegion(ctx, actor, "disable-"+suffix, RegionMutation{Action: "disable", ID: region.ID, Revision: region.Revision, Reason: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if w := observe(rotated.RuntimeCredential, "epoch-2", 4, true); w.Code != 401 {
		t.Fatal("disabled region authorized runtime", w.Code)
	}
	var eligible bool
	if err = store.Pool().QueryRow(ctx, `SELECT node_pool_eligible($1,$2,'relay')`, input.NodeID, actor).Scan(&eligible); err != nil || eligible {
		t.Fatal("disabled region selected", err)
	}
	if _, err = service.RuntimeCredential(ctx, input.NodeID); err == nil || errors.Is(err, nodeauth.ErrNotFound) {
		t.Fatal("disabled scoped credential could fall back to fleet", err)
	}
	if _, err = service.MutateRegion(ctx, actor, "enable-"+suffix, RegionMutation{Action: "enable", ID: region.ID, Revision: disabled.Revision, Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err = service.MutateNode(ctx, actor, "stale-resume-"+suffix, NodeMutation{Action: "resume", NodeID: input.NodeID, Revision: resumed.Node.Revision, Reason: "test"}); !errors.Is(err, ErrConflict) {
		t.Fatal("pre-disable revision accepted", err)
	}
	page, err = service.ListNodes(ctx, actor, region.ID, "", 1)
	if err != nil || page.Items[0].Ready {
		t.Fatal("reenable silently resumed node", err)
	}
	// A process shutdown withdraws readiness without becoming an operator drain.
	resumeAfterEnable, err := service.MutateNode(ctx, actor, "resume-after-enable-"+suffix, NodeMutation{Action: "resume", NodeID: input.NodeID, Revision: page.Items[0].Revision, Reason: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if w := observe(rotated.RuntimeCredential, "epoch-2", 4, false); w.Code != 200 {
		t.Fatal("ready before process shutdown", w.Code)
	}
	for range 2 {
		if w := observe(rotated.RuntimeCredential, "epoch-2", 4, true); w.Code != 200 {
			t.Fatal("repeated process shutdown", w.Code)
		}
	}
	if w := observe(rotated.RuntimeCredential, "epoch-2", 4, false); w.Code != 409 {
		t.Fatal("shutdown process revived itself", w.Code)
	}
	if w := start(rotated.RuntimeCredential, "epoch-3", 4); w.Code != 200 {
		t.Fatal("fresh process cannot restart after shutdown", w.Code)
	}
	if w := observe(rotated.RuntimeCredential, "epoch-2", 4, false); w.Code != 409 {
		t.Fatal("old process survived replacement", w.Code)
	}
	if w := observe(rotated.RuntimeCredential, "epoch-3", 5, false); w.Code != 200 {
		t.Fatal("restarted process not ready", w.Code)
	}
	operatorDrain, err := service.MutateNode(ctx, actor, "drain-restarted-"+suffix, NodeMutation{Action: "drain", NodeID: input.NodeID, Revision: resumeAfterEnable.Node.Revision, Reason: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if w := observe(rotated.RuntimeCredential, "epoch-3", 5, true); w.Code != 200 {
		t.Fatal("operator drain observation", w.Code)
	}
	if w := start(rotated.RuntimeCredential, "epoch-4", 5); w.Code != 409 {
		t.Fatal("process shutdown cleared operator drain", w.Code)
	}
	_, err = service.MutateNode(ctx, actor, "resume-restarted-"+suffix, NodeMutation{Action: "resume", NodeID: input.NodeID, Revision: operatorDrain.Node.Revision, Reason: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if w := start(rotated.RuntimeCredential, "epoch-4", 5); w.Code != 200 {
		t.Fatal("operator resume failed to permit restart", w.Code)
	}
	page, err = service.ListNodes(ctx, actor, region.ID, "", 1)
	if err != nil || len(page.Items) != 1 {
		t.Fatal("read before retire", err)
	}
	retired, err := service.MutateNode(ctx, actor, "retire-"+suffix, NodeMutation{Action: "retire", NodeID: input.NodeID, Revision: page.Items[0].Revision, Reason: "test"})
	if err != nil || retired.Node.State != "retired" {
		t.Fatal("retire", err)
	}
	if w := start(rotated.RuntimeCredential, "after-retire", 4); w.Code != 401 {
		t.Fatal("retired credential accepted", w.Code)
	}
}
