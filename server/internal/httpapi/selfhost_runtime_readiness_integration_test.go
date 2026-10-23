package httpapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/secrets"
	"github.com/pinksaucepasta/paperboat/server/internal/selfhost"
)

func TestSelfhostRuntimeReadinessAuthenticatesExactNodeOnPostgres(t *testing.T) {
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
	t.Cleanup(func() { store.Close() })
	ctx := t.Context()
	if err := db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	prefix := "readiness_" + uuid.NewString()
	key := strings.Repeat("k", 32)
	ids := []string{prefix + "_one", prefix + "_two"}
	credentials := []string{prefix + "_credential_one", prefix + "_credential_two"}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := store.SQL().ExecContext(cleanup, `DELETE FROM paperboat.selfhost_installations WHERE node_id=ANY($1::text[])`, ids); err != nil {
			t.Error("installation cleanup failed")
		}
		if _, err := store.SQL().ExecContext(cleanup, `DELETE FROM paperboat.control_tunnel_nodes WHERE id=ANY($1::text[])`, ids); err != nil {
			t.Error("node cleanup failed")
		}
	})
	for i, id := range ids {
		ciphertext, err := secrets.Encrypt(key, credentials[i])
		if err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256([]byte(credentials[i]))
		public, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,endpoint_host,endpoint_tcp_port,endpoint_quic_port,state,ready,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,tls_spki_sha256,last_heartbeat_at,version) VALUES($1,$1,'1.0',$2,'relay.example.test',27445,27446,'ready',true,'test','test-a',ARRAY['relay'],ARRAY['derp_quic','derp_wss'],8,0,now(),now()+interval '1 hour',decode(repeat('00',32),'hex'),now(),1)`, id, id+"_epoch"); err != nil {
			t.Fatal(err)
		}
		if _, err := store.SQL().ExecContext(ctx, `INSERT INTO paperboat.selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete) VALUES($1,$1,$4,$2,$3,'Readiness test','relay','global','global',$1,true)`, id, hash[:], ciphertext, []byte(public)); err != nil {
			t.Fatal(err)
		}
	}
	router := NewRouter(Options{Selfhost: &SelfhostHandlers{Service: selfhost.New(store, key)}})
	request := func(token, query string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/v1/selfhost/runtime-readiness"+query, nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	for i, id := range ids {
		if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.control_tunnel_nodes SET last_heartbeat_at=now() WHERE id=$1`, id); err != nil {
			t.Fatal(err)
		}
		w := request(credentials[i], "")
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("runtime readiness was cacheable")
		}
		if w.Code != 200 {
			t.Fatalf("own runtime readiness status=%d, want200", w.Code)
		}
		var wire struct {
			Data struct {
				NodeID       string    `json:"node_id"`
				Ready        bool      `json:"ready"`
				ProcessEpoch string    `json:"process_epoch"`
				ValidUntil   time.Time `json:"valid_until"`
				ServerTime   time.Time `json:"server_time"`
			} `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &wire); err != nil {
			t.Fatal("invalid readiness response")
		}
		if wire.Data.NodeID != id || !wire.Data.Ready || wire.Data.ProcessEpoch != id+"_epoch" || !wire.Data.ValidUntil.After(wire.Data.ServerTime) {
			t.Fatal("readiness was not fenced to the authenticated current node")
		}
		if bytes.Contains(w.Body.Bytes(), []byte(credentials[i])) || bytes.Contains(w.Body.Bytes(), []byte("runtime_ciphertext")) {
			t.Fatal("runtime credential material was disclosed")
		}
	}
	for _, c := range []struct {
		token, query string
		status       int
	}{{"", "", 401}, {strings.Repeat("z", 48), "", 401}, {credentials[0], "?node_id=" + ids[1], 400}} {
		if w := request(c.token, c.query); w.Code != c.status {
			t.Fatalf("invalid runtime request status=%d, want%d", w.Code, c.status)
		}
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.control_tunnel_nodes SET last_heartbeat_at=now()-interval '1 hour' WHERE id=$1`, ids[0]); err != nil {
		t.Fatal(err)
	}
	w := request(credentials[0], "")
	var stale struct {
		Data struct {
			Ready bool `json:"ready"`
		} `json:"data"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &stale) != nil || stale.Data.Ready {
		t.Fatal("stale node was reported ready")
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.selfhost_installations SET claim_complete=false WHERE id=$1`, ids[0]); err != nil {
		t.Fatal(err)
	}
	if w := request(credentials[0], ""); w.Code != 401 {
		t.Fatalf("incomplete claim status=%d, want401", w.Code)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.selfhost_installations SET claim_complete=true,expires_at=now()-interval '1 second' WHERE id=$1`, ids[0]); err != nil {
		t.Fatal(err)
	}
	if w := request(credentials[0], ""); w.Code != 401 {
		t.Fatalf("expired credential status=%d, want401", w.Code)
	}
	if _, err := store.SQL().ExecContext(ctx, `UPDATE paperboat.selfhost_installations SET state='removed' WHERE id=$1`, ids[1]); err != nil {
		t.Fatal(err)
	}
	if w := request(credentials[1], ""); w.Code != 401 {
		t.Fatalf("removed node status=%d, want401", w.Code)
	}
}
