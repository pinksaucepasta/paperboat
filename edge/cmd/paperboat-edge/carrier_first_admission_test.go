package main

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/edge/internal/config"
	"github.com/pinksaucepasta/paperboat/edge/internal/datacarrier"
	"github.com/pinksaucepasta/paperboat/edge/internal/edgehttp"
	edgeruntime "github.com/pinksaucepasta/paperboat/edge/internal/runtime"
)

// A cold-started edge opens its carrier listeners before the route worker's first reconcile has
// installed the durable admissions, and refused every machine that redialed in that first minute
// (dev edges 2026-11-13: ~190-280 no_admissions/stale_identity/unknown_machine refusals). A carrier
// that arrives before the first admission reconcile waits for it, bounded, and is then accepted on
// its first dial.
func TestCarrierArrivingBeforeFirstAdmissionWaitsForIt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	golden, err := os.ReadFile(filepath.Join(sharedTestdata, "engine_route_snapshot.json"))
	if err != nil {
		t.Fatal(err)
	}
	server := goldenRouteServer(t, golden)
	var probe struct {
		Routes []struct {
			Node  string `json:"edge_node_id"`
			Epoch string `json:"edge_process_epoch"`
		} `json:"routes"`
	}
	if err := json.Unmarshal(golden, &probe); err != nil || len(probe.Routes) != 1 {
		t.Fatalf("golden: %v", err)
	}
	node, epoch := probe.Routes[0].Node, probe.Routes[0].Epoch
	snapshot, err := server.DesiredRouteSnapshot(ctx, node, epoch)
	if err != nil || len(snapshot.Routes) != 1 {
		t.Fatalf("route decoder: %+v %v", snapshot, err)
	}
	admissions, err := edgeruntime.CanonicalDurableAdmissions(snapshot.Routes)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := os.ReadFile(filepath.Join(sharedTestdata, "engine_machine_seed.hex"))
	if err != nil {
		t.Fatal(err)
	}
	seedBytes, err := hex.DecodeString(strings.TrimSpace(string(seed)))
	if err != nil {
		t.Fatal(err)
	}
	private := ed25519.NewKeyFromSeed(seedBytes)
	public := private.Public().(ed25519.PublicKey)
	durable, err := datacarrier.NewDurableAdmissionRegistry(datacarrier.DurableAdmissionRegistryConfig{NodeID: node, ProcessEpoch: epoch})
	if err != nil {
		t.Fatal(err)
	}
	defer durable.Close()
	routes, err := edgehttp.NewDataCarrierRouteRegistry(edgehttp.DataCarrierRouteRegistryConfig{MaximumRoutes: 8})
	if err != nil {
		t.Fatal(err)
	}
	defer routes.Close()
	tcpAddress, quicAddress := dynamicCarrierListenAddresses(t)
	firstAdmission := make(chan struct{})
	component, closeService, err := newCarrierComponentWithTelemetry(
		config.Deployment{CarrierTCPListenAddress: tcpAddress, CarrierQUICListenAddress: quicAddress, ControlInterval: time.Hour, NodeCapacity: 8},
		dynamicCarrierCertificate(t, "edge-carrier", nil, nil), durable, nil, routes, nil, nil, nil, firstAdmission,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer closeService()
	if err := component.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		stop, done := context.WithTimeout(context.Background(), time.Second)
		defer done()
		_ = component.Shutdown(stop)
	}()
	// The first reconcile lands while the carrier's handshake is waiting.
	go func() {
		time.Sleep(300 * time.Millisecond)
		if err := durable.Replace(admissions, time.Now().UTC()); err == nil {
			close(firstAdmission)
		}
	}()
	dialEngineAgent(t, ctx, tcpAddress, public, private, admissions[0].Identity, epoch)
	waitDynamicCondition(t, ctx, func() bool { return routes.Len() == 1 })
}
