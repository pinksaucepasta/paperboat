package controlplane

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/selfhost"
)

type testBandwidthDebiter struct{ calls int }

func (d *testBandwidthDebiter) DebitEnvironmentBandwidthTx(context.Context, *db.Tx, string, int64, time.Time) (int64, bool, error) {
	d.calls++
	return 0, false, nil
}

func TestSelfhostUsageCannotChargeOrNameOtherResources(t *testing.T) {
	store := openControlPlaneTestDB(t)
	ctx := t.Context()
	now := time.Now().UTC()
	suffix := fmt.Sprint(now.UnixNano())
	seedUsageScope(t, store, suffix)
	account, node, installation := "usage_account_"+suffix, "node_"+suffix, "usage_install_"+suffix
	pub, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES($1,$1,$2,'active')`, account, account+"@example.test")
	t.Cleanup(func() {
		for _, q := range []string{`DELETE FROM paperboat.selfhost_installations WHERE id=$1`} {
			store.SQL().ExecContext(context.Background(), q, installation)
		}
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.control_usage_verification_keys WHERE edge_node_id=$1`, node)
		store.SQL().ExecContext(context.Background(), `UPDATE paperboat.control_environments SET owner_user_id=NULL WHERE id=$1`, "env_"+suffix)
		store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.users WHERE id=$1`, account)
	})
	exec(`INSERT INTO paperboat.selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete) VALUES($1,$2,$3,$3,'ciphertext','usage test','tunnel','account',$4,$1,true)`, installation, node, []byte(pub), account)
	exec(`UPDATE paperboat.control_tunnel_nodes SET tls_spki_sha256=$2 WHERE id=$1`, node, make([]byte, 32))
	exec(`UPDATE paperboat.control_environments SET owner_user_id=$2 WHERE id=$1`, "env_"+suffix, account)
	exec(`UPDATE paperboat.control_routes SET applied_node_id=$2,applied_revision=1 WHERE id=$1`, "route_"+suffix, node)
	exec(`INSERT INTO paperboat.control_usage_verification_keys(key_id,edge_node_id,public_key,not_before,expires_at) VALUES($1,$2,$3,$4,$5)`, installation, node, []byte(pub), now.Add(-time.Hour), now.Add(time.Hour))
	service := NewEdgeService(store, "managed")
	service.SetNodeCredentials(selfhost.New(store, "selfhost-test-encryption-key-32-bytes-long"))
	debiter := &testBandwidthDebiter{}
	service.SetBandwidthDebiter(debiter)
	request := edgeUsageRequest{OperationID: "usage_" + suffix, Node: node, Epoch: "epoch_" + suffix, Environment: "env_" + suffix, Route: "route_" + suffix, Revision: 1, Direction: "egress", Bytes: 25, Start: now.Add(-time.Second), End: now}
	sign := func() {
		payload, _ := json.Marshal(signedUsageDocument{OperationID: request.OperationID, Key: signedUsageKey{Node: request.Node, Epoch: request.Epoch, Environment: request.Environment, Route: request.Route, Revision: request.Revision, Direction: request.Direction}, Bytes: request.Bytes, Start: request.Start, End: request.End})
		request.Payload, _ = json.Marshal(signedUsageEnvelope{Algorithm: "EdDSA", KeyID: installation, Payload: base64.RawURLEncoding.EncodeToString(payload), Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, payload))})
	}
	sign()
	receipt, err := service.Usage(ctx, request)
	if err != nil || receipt.DeltaBytes != 25 {
		t.Fatalf("own usage: %v", err)
	}
	if debiter.calls != 0 {
		t.Fatal("operator report debited Paperboat bandwidth")
	}
	for _, scope := range []string{"team", "global", "account"} {
		scopeID := account
		if scope == "team" {
			scopeID = "usage_team_" + suffix
		}
		if scope == "global" {
			scopeID = "global"
		}
		if scope == "team" {
			exec(`INSERT INTO paperboat.teams(team_id,owner_account,generation) VALUES($1,$2,1)`, scopeID, account)
			exec(`INSERT INTO paperboat.team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true)`, scopeID, account)
			t.Cleanup(func() {
				store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.team_members WHERE team_id=$1`, scopeID)
				store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.teams WHERE team_id=$1`, scopeID)
			})
		}
		exec(`UPDATE paperboat.selfhost_installations SET scope_kind=$2,scope_id=$3 WHERE id=$1`, installation, scope, scopeID)
		exempt, err := service.authorizeSelfhostUsage(ctx, request)
		if err != nil || exempt != (scope != "global") {
			t.Fatalf("usage classification %s exempt=%v err=%v", scope, exempt, err)
		}
	}
	request.OperationID += "_other"
	request.Environment = "other_environment"
	sign()
	if _, err = service.Usage(ctx, request); !errors.Is(err, ErrUsageSignature) {
		t.Fatalf("other environment accepted: %v", err)
	}
	request.Environment = "env_" + suffix
	exec(`UPDATE paperboat.selfhost_installations SET state='removed',claim_complete=false WHERE id=$1`, installation)
	sign()
	if _, err = service.Usage(ctx, request); !errors.Is(err, ErrUsageSignature) {
		t.Fatalf("removed scoped installation usage: %v", err)
	}
}
