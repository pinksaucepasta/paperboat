package controlplane

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/shared/nodemesh"
)

// meshFixture seeds managed and self-hosted nodes under one prefix and region, and removes them at the end.
type meshFixture struct {
	t              *testing.T
	store          *db.DB
	prefix, region string
}

func newMeshFixture(t *testing.T, name string) *meshFixture {
	store := openControlPlaneTestDB(t)
	x := fmt.Sprint(time.Now().UnixNano())
	f := &meshFixture{t: t, store: store, prefix: "zz_" + name + "_" + x + "_", region: name + "_" + x}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.selfhost_installations WHERE node_id LIKE $1`, f.prefix+"%")
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.control_tunnel_nodes WHERE id LIKE $1`, f.prefix+"%")
		_, _ = store.SQL().ExecContext(bg, `DELETE FROM paperboat.operator_regions WHERE id LIKE $1`, f.region+"%")
	})
	f.exec(`INSERT INTO paperboat.operator_regions(id,display_name,enabled) VALUES($1,'Test',true),($2,'Disabled',false)`, f.region, f.region+"_off")
	return f
}

func (f *meshFixture) exec(query string, args ...any) {
	f.t.Helper()
	if _, err := f.store.SQL().ExecContext(context.Background(), query, args...); err != nil {
		f.t.Fatal(err)
	}
}

// pin is the carrier pin of node id's fixture certificate.
func pin(id string) string { s := sha256.Sum256([]byte(id)); return nodemesh.PinFromDigest(s[:]) }

// node inserts a node row; managed adds its managed authority in region.
func (f *meshFixture) node(suffix, role, state string, managed bool, region string, heartbeatAge time.Duration, mutate string) string {
	id := f.prefix + suffix
	now := time.Now().UTC()
	nodeRegion := region
	if nodeRegion == "" { // a node's region is never empty; a self-hosted one is just in some other region
		nodeRegion = "other"
	}
	transports := "http3','http2"
	if role == "relay" {
		transports = "derp_quic','derp_wss"
	}
	f.exec(`INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,state,ready,last_heartbeat_at,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,node_generation,endpoint_host,endpoint_tcp_port,endpoint_quic_port,carrier_endpoint_host,carrier_endpoint_tcp_port,carrier_endpoint_quic_port,carrier_server_spki_sha256,carrier_server_certificate_chain_pem,tls_spki_sha256)
VALUES($1,'default','1.0',$2,$3,$3='ready',$4,$11,$1,ARRAY[$5]::text[],ARRAY['`+transports+`']::text[],128,0,$4,$6,1,$7,443,444,$8,7272,7273,$9,'fixture certificate',$10)`,
		id, "epoch_"+suffix, state, now.Add(-heartbeatAge), role, now.Add(time.Hour), suffix+".node.example.test", "carrier-"+suffix+".node.example.test", pin(id), sha256Bytes(id), nodeRegion)
	if managed {
		h := sha256.Sum256([]byte("runtime:" + id))
		capability := "tunnel"
		if role == "relay" {
			capability = "relay"
		}
		f.exec(`INSERT INTO paperboat.selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete) VALUES($1,$1,decode(md5($1)||md5($1),'hex'),$2,'encrypted','managed fixture',$3,'global','global',$1,true)`, id, h[:], capability)
	}
	// A managed relay reports its serving certificate pin in its observation (the mesh reads it there).
	if role == "relay" && managed {
		f.exec(`UPDATE paperboat.control_tunnel_nodes SET observation=jsonb_build_object('serving_spki_sha256',$2::text) WHERE id=$1`, id, pin(id))
	}
	if mutate != "" {
		f.exec(`UPDATE paperboat.control_tunnel_nodes SET `+mutate+` WHERE id=$1`, id)
	}
	return id
}

func sha256Bytes(s string) []byte { h := sha256.Sum256([]byte(s)); return h[:] }

func peerIDs(peers []nodemesh.Peer) map[string]nodemesh.Peer {
	out := map[string]nodemesh.Peer{}
	for _, p := range peers {
		out[p.NodeID] = p
	}
	return out
}

func TestPeersListsManagedReadyAndDrainingEdgesOnly(t *testing.T) {
	f := newMeshFixture(t, "peers")
	caller := f.node("a", "edge", "ready", true, f.region, 0, "")
	ready := f.node("b", "edge", "ready", true, f.region, 5*time.Second, "public_ingress_ipv4='192.0.2.10', public_ingress_verified_at=now()")
	draining := f.node("c", "edge", "draining", true, f.region, 0, "public_https_port=8443, ready=false")
	f.node("retired", "edge", "ready", true, f.region, 0, "")
	f.exec(`UPDATE paperboat.control_tunnel_nodes SET state='retired' WHERE id=$1`, f.prefix+"retired")
	f.node("stale", "edge", "ready", true, f.region, 3*time.Minute, "")
	f.node("relay", "relay", "ready", true, f.region, 0, "")
	f.node("nopin", "edge", "ready", true, f.region, 0, "carrier_server_spki_sha256=NULL")
	f.node("selfhosted", "edge", "ready", false, "", 0, "")
	f.node("off", "edge", "ready", true, f.region+"_off", 0, "")
	f.node("registered", "edge", "registered", true, f.region, 0, "")
	s := NewEdgeService(f.store, "unused")
	got, err := s.EdgePeers(t.Context(), caller, "epoch_a")
	if err != nil {
		t.Fatal(err)
	}
	if got.Region != f.region {
		t.Fatalf("answer region = %q, want the caller's own region %q", got.Region, f.region)
	}
	peers := peerIDs(got.Peers)
	if len(got.Peers) != 2 || len(peers) != 2 || peers[ready].NodeID == "" || peers[draining].NodeID == "" {
		t.Fatalf("peers=%+v, want only the ready and the draining managed edge (not self, retired, stale, relay, no pin, self-hosted, disabled region, registered)", got.Peers)
	}
	if p := peers[ready]; p.Host != "192.0.2.10" || p.Port != 443 || p.Draining || p.SPKISHA256 != pin(ready) || p.Region != f.region || p.ProcessEpoch != "epoch_b" {
		t.Fatalf("ready peer %+v: want the verified ingress address, port 443, the registered carrier pin", p)
	}
	if p := peers[draining]; p.Host != "carrier-c.node.example.test" || p.Port != 8443 || !p.Draining || p.SPKISHA256 != pin(draining) {
		t.Fatalf("draining peer %+v: want the carrier host (no verified ingress), its published port, draining", p)
	}
	for _, p := range got.Peers {
		if err := p.Validate(); err != nil {
			t.Fatalf("peer %+v: %v", p, err)
		}
	}
	if _, err := s.EdgePeers(t.Context(), caller, "epoch_other"); err == nil || !strings.Contains(err.Error(), "current process") {
		t.Fatalf("a replaced process got the directory: %v", err)
	}
}

func TestPeersSelfHostedCallerGetsNone(t *testing.T) {
	f := newMeshFixture(t, "peerssh")
	f.node("m", "edge", "ready", true, f.region, 0, "")
	own := f.node("own", "edge", "ready", false, "", 0, "")
	f.node("own2", "edge", "ready", false, "", 0, "")
	got, err := NewEdgeService(f.store, "unused").EdgePeers(t.Context(), own, "epoch_own")
	if err != nil || got.Peers == nil || len(got.Peers) != 0 {
		t.Fatalf("self-hosted caller peers=%+v err=%v, want an empty list (never the managed nodes, never another self-hosted one)", got.Peers, err)
	}
}

func TestHeartbeatStoresForwardStatsAndRelayObserveStoresMesh(t *testing.T) {
	f := newMeshFixture(t, "stats")
	edge := f.node("e", "edge", "ready", true, f.region, 0, "")
	s := NewEdgeService(f.store, "unused")
	want := nodemesh.Stats{Links: 2, Names: 7, Forwarded: 11, Accepted: 5, HolderMiss: 1, Unreachable: 2, LoopRefused: 3, BytesIn: 100, BytesOut: 200}
	if _, err := s.Heartbeat(t.Context(), edgeNodeObservation{NodeID: edge, ProcessEpoch: "epoch_e", Ready: true, At: time.Now(), Forward: &want}); err != nil {
		t.Fatal(err)
	}
	var stored string
	f.query(&stored, `SELECT observation->'forward'->>'forwarded'||'/'||(observation->'forward'->>'names') FROM paperboat.control_tunnel_nodes WHERE id=$1`, edge)
	if stored != "11/7" {
		t.Fatalf("stored forward = %q, want 11/7", stored)
	}
	if _, err := s.Heartbeat(t.Context(), edgeNodeObservation{NodeID: edge, ProcessEpoch: "epoch_e", Ready: true, At: time.Now(), Forward: &nodemesh.Stats{Links: -1}}); err == nil {
		t.Fatal("negative link count accepted")
	}
	if _, err := s.Heartbeat(t.Context(), edgeNodeObservation{NodeID: edge, ProcessEpoch: "epoch_e", Ready: true, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	var has bool
	f.query(&has, `SELECT observation ? 'forward' FROM paperboat.control_tunnel_nodes WHERE id=$1`, edge)
	if has {
		t.Fatal("an edge that reports no forward counters keeps the old ones")
	}

	relay := f.node("r", "relay", "ready", true, f.region, 0, "")
	if _, err := s.ObserveRelayNode(t.Context(), relayNodeObservation{NodeID: relay, Generation: 1, ProcessEpoch: "epoch_r", Ready: true, Mesh: &want}); err != nil {
		t.Fatal(err)
	}
	f.query(&stored, `SELECT observation->'mesh'->>'accepted' FROM paperboat.control_tunnel_nodes WHERE id=$1`, relay)
	if stored != "5" {
		t.Fatalf("stored mesh accepted = %q, want 5", stored)
	}
	if _, err := s.ObserveRelayNode(t.Context(), relayNodeObservation{NodeID: relay, Generation: 1, ProcessEpoch: "epoch_r", Ready: true}); err != nil {
		t.Fatal(err)
	}
	f.query(&has, `SELECT observation ? 'mesh' FROM paperboat.control_tunnel_nodes WHERE id=$1`, relay)
	if has {
		t.Fatal("a relay that reports no mesh counters keeps the old ones")
	}
}

func (f *meshFixture) query(dest any, query string, args ...any) {
	f.t.Helper()
	if err := f.store.SQL().QueryRowContext(context.Background(), query, args...).Scan(dest); err != nil {
		f.t.Fatal(err)
	}
}

func TestRelayLeaseCarriesMeshPeers(t *testing.T) {
	f := newMeshFixture(t, "mesh")
	self := f.node("self", "relay", "ready", true, f.region, 0, "")
	ready := f.node("ready", "relay", "ready", true, f.region, 5*time.Second, "endpoint_quic_port=9444")
	draining := f.node("drain", "relay", "draining", true, f.region, 0, "drain_deadline=now()+interval '30 seconds'")
	f.node("stale", "relay", "ready", true, f.region, 3*time.Minute, "")
	f.node("nopin", "relay", "ready", true, f.region, 0, "observation='{}'::jsonb")
	f.node("edge", "edge", "ready", true, f.region, 0, "")
	f.node("selfhosted", "relay", "ready", false, "", 0, "")
	f.node("off", "relay", "ready", true, f.region+"_off", 0, "")
	f.node("offline", "relay", "offline", true, f.region, 0, "")
	s := NewEdgeService(f.store, "unused")
	lease, err := s.ObserveRelayNode(t.Context(), relayNodeObservation{NodeID: self, Generation: 1, ProcessEpoch: "epoch_self", Ready: true})
	if err != nil {
		t.Fatal(err)
	}
	peers := peerIDs(lease.MeshPeers)
	if len(lease.MeshPeers) != 2 || peers[ready].NodeID == "" || peers[draining].NodeID == "" {
		t.Fatalf("mesh peers %+v, want only the ready and the draining managed relay", lease.MeshPeers)
	}
	if p := peers[ready]; p.Host != "ready.node.example.test" || p.Port != 9444 || p.Draining || p.SPKISHA256 != nodemesh.PinFromDigest(sha256Bytes(ready)) || p.Region != f.region {
		t.Fatalf("ready relay peer %+v: want its endpoint, QUIC port and the pin of its serving certificate", p)
	}
	if !peers[draining].Draining {
		t.Fatalf("draining relay peer %+v", peers[draining])
	}
	// A customer-owned (not managed) relay is given none, and a managed one never lists it.
	own := f.node("own", "relay", "ready", false, "", 0, "")
	f.exec(`UPDATE paperboat.control_tunnel_nodes SET state='ready' WHERE id=$1`, own)
	lease, err = s.ObserveRelayNode(t.Context(), relayNodeObservation{NodeID: own, Generation: 1, ProcessEpoch: "epoch_own", Ready: true})
	if err != nil || len(lease.MeshPeers) != 0 {
		t.Fatalf("self-hosted relay mesh peers=%+v err=%v, want none", lease.MeshPeers, err)
	}
	// A self-hosted relay that reports a pin is still never a managed relay's peer.
	f.exec(`UPDATE paperboat.control_tunnel_nodes SET observation=jsonb_build_object('serving_spki_sha256',$2::text) WHERE id=$1`, own, pin(own))
	lease, err = s.ObserveRelayNode(t.Context(), relayNodeObservation{NodeID: self, Generation: 1, ProcessEpoch: "epoch_self", Ready: true})
	if _, listed := peerIDs(lease.MeshPeers)[own]; err != nil || listed {
		t.Fatalf("managed relay mesh peers %+v err=%v, want the self-hosted relay left out", lease.MeshPeers, err)
	}
	// A rotated pin replaces the reported one in the peers other relays get.
	rotated := nodemesh.PinFromDigest(sha256Bytes("rotated"))
	if _, err := s.ObserveRelayNode(t.Context(), relayNodeObservation{NodeID: ready, Generation: 1, ProcessEpoch: "epoch_ready", Ready: true, ServingSPKISHA256: rotated}); err != nil {
		t.Fatal(err)
	}
	lease, err = s.ObserveRelayNode(t.Context(), relayNodeObservation{NodeID: self, Generation: 1, ProcessEpoch: "epoch_self", Ready: true})
	if err != nil || peerIDs(lease.MeshPeers)[ready].SPKISHA256 != rotated {
		t.Fatalf("mesh peers after rotation %+v err=%v, want the ready relay's new pin", lease.MeshPeers, err)
	}
	// A start that reports no pin keeps the stored one.
	if _, err := s.StartRelayNode(t.Context(), relayNodeStart{NodeID: ready, ExpectedGeneration: 1, ProcessEpoch: "epoch_ready2"}); err != nil {
		t.Fatal(err)
	}
	var stored string
	f.query(&stored, `SELECT observation->>'serving_spki_sha256' FROM paperboat.control_tunnel_nodes WHERE id=$1`, ready)
	if stored != rotated {
		t.Fatalf("stored pin after a start without one = %q, want %q", stored, rotated)
	}
}

func TestRelayObserveStoresServingPin(t *testing.T) {
	f := newMeshFixture(t, "servingpin")
	relay := f.node("r", "relay", "ready", true, f.region, 0, "observation='{}'::jsonb")
	s := NewEdgeService(f.store, "unused")
	observe := func(reported string) error {
		_, err := s.ObserveRelayNode(t.Context(), relayNodeObservation{NodeID: relay, Generation: 1, ProcessEpoch: "epoch_r", Ready: true, ServingSPKISHA256: reported})
		return err
	}
	stored := func() string {
		var got string
		f.query(&got, `SELECT coalesce(observation->>'serving_spki_sha256','') FROM paperboat.control_tunnel_nodes WHERE id=$1`, relay)
		return got
	}
	if err := observe("sha256:" + strings.Repeat("zz", 32)); !errors.Is(err, ErrInvalidUsageReport) {
		t.Fatalf("malformed serving pin: err %v, want invalid usage", err)
	}
	if got := stored(); got != "" {
		t.Fatalf("a rejected serving pin was stored: %q", got)
	}
	// The pin is stored in canonical lower-case form, and an observation without one keeps it.
	if err := observe(strings.ToUpper(pin(relay))); err != nil {
		t.Fatal(err)
	}
	if got := stored(); got != pin(relay) {
		t.Fatalf("stored serving pin %q, want %q", got, pin(relay))
	}
	if err := observe(""); err != nil {
		t.Fatal(err)
	}
	if got := stored(); got != pin(relay) {
		t.Fatalf("observation without a pin changed the stored pin to %q", got)
	}
}

// TestRelayMeshPeersOmitsRepeatedPin: a managed relay that reports another relay's pin makes both unlisted, so the
// directory every relay receives stays free of a repeated pin (a relay refuses such a directory whole).
func TestRelayMeshPeersOmitsRepeatedPin(t *testing.T) {
	f := newMeshFixture(t, "duppin")
	self := f.node("self", "relay", "ready", true, f.region, 0, "")
	honest := f.node("honest", "relay", "ready", true, f.region, 0, "")
	other := f.node("other", "relay", "ready", true, f.region, 0, "")
	s := NewEdgeService(f.store, "unused")
	lease, err := s.ObserveRelayNode(t.Context(), relayNodeObservation{NodeID: self, Generation: 1, ProcessEpoch: "epoch_self", Ready: true})
	if err != nil || len(lease.MeshPeers) != 2 {
		t.Fatalf("mesh peers %+v err=%v, want both relays", lease.MeshPeers, err)
	}
	if _, err := s.ObserveRelayNode(t.Context(), relayNodeObservation{NodeID: other, Generation: 1, ProcessEpoch: "epoch_other", Ready: true, ServingSPKISHA256: pin(honest)}); err != nil {
		t.Fatal(err)
	}
	lease, err = s.ObserveRelayNode(t.Context(), relayNodeObservation{NodeID: self, Generation: 1, ProcessEpoch: "epoch_self", Ready: true})
	if err != nil || len(lease.MeshPeers) != 0 {
		t.Fatalf("mesh peers %+v err=%v, want neither relay listed under the repeated pin", lease.MeshPeers, err)
	}
}
