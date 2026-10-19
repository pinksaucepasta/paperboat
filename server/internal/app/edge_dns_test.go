package app

import (
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/db/dbsqlc"
)

func TestRegionalDNSProjectionFencesReadyIdentity(t *testing.T) {
	now := time.Now().UTC()
	rows := []dbsqlc.ListReadyEdgeDNSAddressesV1Row{
		{ID: "edge_a", ResourceGeneration: 7, ObservedAt: now, Ipv4: "8.8.8.8", ReadinessVersion: "epoch_a/assignment_1/certificate_1", ValidUntil: now.Add(10 * time.Second)},
		{ID: "edge_b", ResourceGeneration: 7, ObservedAt: now, Ipv4: "1.1.1.1", Ipv6: "2606:4700:4700::1111", ReadinessVersion: "epoch_b/assignment_2/certificate_1", ValidUntil: now.Add(8 * time.Second)},
	}
	d, err := readyEdgePublication("tunnel_1", "stable.example.test", 7, rows)
	if err != nil || len(d.Addresses) != 3 || !d.IPv6ReachabilityVerified || !d.ValidUntil.Equal(rows[1].ValidUntil) {
		t.Fatalf("ready projection: %+v %v", d, err)
	}
	before := d.ReadinessVersion
	rows[1].ReadinessVersion = "epoch_replaced/assignment_3/certificate_1"
	d, err = readyEdgePublication("tunnel_1", "stable.example.test", 7, rows)
	if err != nil || d.ReadinessVersion == before {
		t.Fatal("process/assignment replacement reused readiness")
	}
	rows[1].ResourceGeneration = 8
	if _, err = readyEdgePublication("tunnel_1", "stable.example.test", 7, rows); err == nil {
		t.Fatal("mixed resource generation published")
	}
	rows[1].ResourceGeneration = 7
	if _, err = readyEdgePublication("tunnel_1", "stable.example.test", 7, append(rows, rows[0])); err == nil {
		t.Fatal("unbounded placement published")
	}
	d, err = readyEdgePublication("tunnel_1", "stable.example.test", 7, nil)
	if err != nil || len(d.Addresses) != 0 {
		t.Fatal("unavailable resource needs empty withdrawal")
	}
}

func TestPrioritizeEdgeDNSOwnersBoundsAStalledOwner(t *testing.T) {
	now := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	since := map[string]time.Time{}
	pending := []dbsqlc.ListPendingEdgeDNSPublicationResourcesV1Row{{ID: "preview_a", Generation: 1}, {ID: "tunnel_b", Generation: 3}}
	if got := prioritizeEdgeDNSOwners(now, since, pending); len(got) != 2 {
		t.Fatalf("new pending owners must be prioritized, got %v", got)
	}
	if got := prioritizeEdgeDNSOwners(now.Add(pendingEdgeDNSPriorityWindow-time.Second), since, pending[:1]); len(got) != 1 || got[0].ID != "preview_a" {
		t.Fatalf("owner inside the window stays prioritized, got %v", got)
	}
	if _, kept := since["tunnel_b"]; kept {
		t.Fatal("an owner that left the pending set must be forgotten")
	}
	if got := prioritizeEdgeDNSOwners(now.Add(pendingEdgeDNSPriorityWindow), since, pending[:1]); len(got) != 0 {
		t.Fatalf("a stalled owner falls back to round-robin, got %v", got)
	}
	if got := prioritizeEdgeDNSOwners(now.Add(2*pendingEdgeDNSPriorityWindow), since, nil); len(got) != 0 || len(since) != 0 {
		t.Fatalf("settled owners are forgotten, got %v since=%v", got, since)
	}
}
