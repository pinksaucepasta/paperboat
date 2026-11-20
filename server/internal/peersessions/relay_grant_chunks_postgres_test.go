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
	"github.com/pinksaucepasta/paperboat/shared/peernetwork"
)

// An account with 100 peers gets relay grants naming every peer and every peer's relay control key, across
// chunks that released relays and CLIs accept (saw grants naming 16 of 44-96 peers).
func TestRelayGrantOfAHundredPeerAccountIsOneMembershipOnPostgres(t *testing.T) {
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
	if err = db.Migrate(context.Background(), store); err != nil {
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
	service, err := NewNetworkService(store, provider, "https://api.example.test")
	if err != nil {
		t.Fatal(err)
	}
	usePolicyCache(t, service, store)
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
	self := networkBinding{Binding: peernetwork.Binding{AccountID: account, EndpointID: "self", WireGuardPublicKey: key(), DiscoPublicKey: key(), QUICCertificateFingerprint: hex.EncodeToString(make32()), QUICPublicKey: key()}}
	var peers []networkPeer
	want := map[string]bool{}
	for i := range 100 {
		p := networkPeer{Identity: networkBinding{Binding: peernetwork.Binding{AccountID: account, EndpointID: fmt.Sprintf("peer_%03d", i), WireGuardPublicKey: key(), DiscoPublicKey: key()}},
			Scopes: []networkScope{{ResourceKind: "machine_network", ResourceID: fmt.Sprintf("peer_%03d", i), ResourceGeneration: 1, Capability: "connect", Direction: "dial", Port: 443, ExpiresAt: now.Add(time.Hour).Unix()}}}
		peers = append(peers, p)
		want[p.Identity.WireGuardPublicKey] = true
	}
	nodes, err := service.relayNodes(ctx, account, now)
	if err != nil {
		t.Fatal(err)
	}
	grants, err := service.relayGrants(nodes, self, peers, 7, now)
	if err != nil {
		t.Fatal(err)
	}
	// Other tests' relay nodes in a shared database may admit every account: count this node's grants.
	var mine []map[string]any
	size := 0
	for _, grant := range grants {
		if claims := networkClaims(t, grant); claims["node_id"] == node {
			mine, size = append(mine, claims), len(grant)
		}
	}
	// One grant whatever the account size: the account network membership, no peer list or control keys.
	if len(mine) != 1 {
		t.Fatalf("grants for the node = %d, want one", len(mine))
	}
	claims := mine[0]
	memberships, _ := claims["memberships"].([]any)
	peerList, _ := claims["peers"].([]any)
	keys, _ := claims["relay_control_peers"].([]any)
	if claims["generation"].(float64) != 7 || len(peerList) != 0 || len(keys) != 0 || len(memberships) != 1 || claims["peer_relay"] == nil {
		t.Fatalf("grant %v: want generation 7, the peer relay and one membership, no peers", claims)
	}
	m := memberships[0].(map[string]any)
	if m["resource_id"] != peernetwork.AccountNetworkID(account) || m["direction"] != "dial" {
		t.Fatalf("membership %v, want the account network (dial)", m)
	}
	t.Logf("relay grant for a 100-peer endpoint: %d bytes", size)
}

func make32() []byte {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return b
}
