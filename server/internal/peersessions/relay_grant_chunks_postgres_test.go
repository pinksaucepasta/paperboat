package peersessions

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
)

// An account with 100 peers gets relay grants naming every peer and every peer's relay control key, across
// chunks that released relays and CLIs accept (Lane W saw grants naming 16 of 44-96 peers).
func TestRelayGrantsNameEveryPeerOfAHundredPeerAccountOnPostgres(t *testing.T) {
	dsn := os.Getenv("PAPERBOAT_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL required")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = db.Migrate(ctx, store); err != nil {
		t.Fatal(err)
	}
	_, signing, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := mint.New([]mint.Key{{ID: "chunk-test", PrivateKey: signing}}, "chunk-test", mint.MaxProofTTL)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewNetworkService(store, provider, "https://api.example.test", NetworkOptions{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	suffix := fmt.Sprint(now.UnixNano())
	account, node := "chunk_user_"+suffix, "chunk_node_"+suffix
	key := func() string {
		b := make([]byte, 32)
		_, _ = rand.Read(b)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	if _, err = store.SQL().ExecContext(ctx, `INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,endpoint_host,endpoint_tcp_port,endpoint_quic_port,state,ready,last_heartbeat_at,node_generation,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,allowed_account_ids,peer_relay_wireguard_public_key,peer_relay_disco_public_key,peer_relay_virtual_address) VALUES($1,'chunks','v1','epoch',$2,443,444,'ready',true,$3,1,'hel','hel-a',ARRAY['relay','peer_relay'],ARRAY['derp_quic','peer_relay_udp'],100,1,$3,$4,ARRAY[$5],$6,$7,('fd7a:115c:a1e0::'::inet + nextval('paperboat.peer_network_virtual_address_seq')))`, node, node+".example.test", now, now.Add(time.Minute), account, make32(), make32()); err != nil {
		t.Fatal(err)
	}
	defer store.SQL().ExecContext(context.Background(), `DELETE FROM paperboat.control_tunnel_nodes WHERE id=$1`, node)
	self := networkBinding{AccountID: account, EndpointID: "self", WireGuardPublicKey: key(), DiscoPublicKey: key(), QUICCertificateFingerprint: hex.EncodeToString(make32()), QUICPublicKey: key()}
	var peers []networkPeer
	want := map[string]bool{}
	for i := range 100 {
		p := networkPeer{Identity: networkBinding{AccountID: account, EndpointID: fmt.Sprintf("peer_%03d", i), WireGuardPublicKey: key(), DiscoPublicKey: key()},
			Scopes: []networkScope{{ResourceKind: "machine_network", ResourceID: fmt.Sprintf("peer_%03d", i), ResourceGeneration: 1, Capability: "connect", Direction: "dial", Port: 1, ExpiresAt: now.Add(time.Hour).Unix()}}}
		peers = append(peers, p)
		want[p.Identity.WireGuardPublicKey] = true
	}
	tx, err := store.SQL().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	grants, err := service.relayGrants(ctx, tx, self, peers, peers, 7, now, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// Other tests' relay nodes in a shared database may admit every account: count this node's grants.
	var mine []map[string]any
	for _, grant := range grants {
		if claims := networkClaims(t, grant); claims["node_id"] == node {
			mine = append(mine, claims)
		}
	}
	if len(mine) != 7 {
		t.Fatalf("grants for the node = %d, want 7 chunks of at most %d", len(mine), maximumRelayControlPeers)
	}
	named, control := map[string]bool{}, map[string]bool{}
	for _, claims := range mine {
		if claims["generation"].(float64) != 7 {
			t.Fatalf("grant generation = %v", claims["generation"])
		}
		inGrant := map[string]bool{}
		for _, entry := range claims["peers"].([]any) {
			k := entry.(map[string]any)["wireguard_public_key"].(string)
			if named[k] || !want[k] {
				t.Fatalf("peer %s repeated or unknown", k)
			}
			named[k], inGrant[k] = true, true
		}
		keys, _ := claims["relay_control_peers"].([]any)
		if len(keys) > maximumRelayControlPeers {
			t.Fatalf("grant names %d control peers; released relays and CLIs refuse more than %d", len(keys), maximumRelayControlPeers)
		}
		for _, k := range keys {
			if !inGrant[k.(string)] || control[k.(string)] {
				t.Fatalf("control key %v outside its peer's grant or repeated", k)
			}
			control[k.(string)] = true
		}
	}
	if len(named) != 100 || len(control) != 100 {
		t.Fatalf("grants name %d of 100 peers and %d of 100 control keys", len(named), len(control))
	}
}

func make32() []byte {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return b
}
