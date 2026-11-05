package datacarrier

import (
	"testing"
	"time"

	runtimewire "github.com/pinksaucepasta/paperboat/shared/runtimeattachment"
)

// A worker handover keeps the old endpoint admission only for the same machine's newer attachment
// of the same route, and only for supersededGrace; a withdrawal or a move to another machine ends
// it at once.
func TestSupersededEndpointAdmissionIsBoundToTheSameMachinesNewerAttachment(t *testing.T) {
	now := time.Now()
	admission := func(session string, generation uint64, host string) ExpectedAdmission {
		return ExpectedAdmission{Schema: runtimewire.Schema, RouteKind: runtimewire.EndpointPublicRouteKind, RouteID: "route_1", OperationID: "route_1",
			Hostname: "app.preview.example.test", AttachmentGeneration: generation, ExpiresAt: now.Add(2 * time.Minute), Admitted: true,
			Identity: Identity{AccountID: "account_1", HostID: host, SessionID: session, ProcessGeneration: generation}}
	}
	old := admission("session_old", 7, "host_1")
	prior := map[string]ExpectedAdmission{old.key(): old}
	held := supersededBy(prior, nil, map[string]ExpectedAdmission{old.key(): admission("session_new", 8, "host_1")}, now)
	if len(held) != 1 {
		t.Fatalf("handover held %d admissions, want the old one", len(held))
	}
	for _, kept := range held {
		if kept.Identity != old.Identity || !kept.ExpiresAt.Equal(now.Add(supersededGrace)) {
			t.Fatalf("held %+v, want the old admission until the grace ends", kept)
		}
	}
	if again := supersededBy(nil, held, map[string]ExpectedAdmission{old.key(): admission("session_new", 8, "host_1")}, now.Add(supersededGrace)); len(again) != 0 {
		t.Fatal("a superseded admission outlived its grace")
	}
	if withdrawn := supersededBy(nil, held, map[string]ExpectedAdmission{}, now); len(withdrawn) != 0 {
		t.Fatal("a withdrawn route kept its old carrier")
	}
	for name, next := range map[string]ExpectedAdmission{
		"other machine": admission("session_new", 8, "host_2"),
		"older":         admission("session_new", 6, "host_1"),
	} {
		if got := supersededBy(prior, nil, map[string]ExpectedAdmission{old.key(): next}, now); len(got) != 0 {
			t.Fatalf("%s: held the old admission", name)
		}
	}
}

// pb update: the old worker's runtime admission stays for the browser terminals its carrier holds, up
// to DrainLimit, past its own short expiry, while the same machine's newer attachment holds
// the route; a withdrawal (revocation) ends it at once.
func TestSupersededRuntimeAdmissionHoldsBrowserTerminalsUntilTheDrainLimit(t *testing.T) {
	now := time.Now()
	admission := func(session string, generation uint64, host string) ExpectedAdmission {
		return ExpectedAdmission{Schema: runtimewire.Schema, RouteKind: runtimewire.RouteKind, RouteID: "route_1", OperationID: "route_1",
			Hostname: "machine.runtime.example.test", AttachmentGeneration: generation, ExpiresAt: now.Add(2 * time.Minute), Admitted: true,
			Identity: Identity{AccountID: "account_1", HostID: host, SessionID: session, ProcessGeneration: generation}}
	}
	old, next := admission("session_old", 7, "host_1"), admission("session_new", 8, "host_1")
	successor := map[string]ExpectedAdmission{next.key(): next}
	held := supersededBy(map[string]ExpectedAdmission{old.key(): old}, nil, successor, now)
	if len(held) != 1 {
		t.Fatalf("handover held %d runtime admissions, want the old one", len(held))
	}
	for _, kept := range held {
		if kept.Identity != old.Identity || !kept.ExpiresAt.Equal(now.Add(runtimewire.DrainLimit)) {
			t.Fatalf("held %+v, want the old admission until the drain limit", kept)
		}
	}
	if still := supersededBy(nil, held, successor, now.Add(runtimewire.DrainLimit-time.Second)); len(still) != 1 {
		t.Fatal("the old runtime admission ended at its own expiry, before the drain limit")
	}
	if again := supersededBy(nil, held, successor, now.Add(runtimewire.DrainLimit)); len(again) != 0 {
		t.Fatal("a superseded runtime admission outlived the drain limit")
	}
	if revoked := supersededBy(nil, held, map[string]ExpectedAdmission{}, now); len(revoked) != 0 {
		t.Fatal("a withdrawn (revoked) runtime route kept its old carrier")
	}
	if other := supersededBy(map[string]ExpectedAdmission{old.key(): old}, nil, map[string]ExpectedAdmission{old.key(): admission("session_new", 8, "host_2")}, now); len(other) != 0 {
		t.Fatal("another machine's attachment held the old runtime admission")
	}
}
