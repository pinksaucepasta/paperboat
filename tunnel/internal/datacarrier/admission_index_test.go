package datacarrier

import (
	"fmt"
	"testing"
	"time"
)

// Audit T2: a carrier's admissions come from an index rebuilt on each change, not a scan of every
// admission per carrier per poll; Changed wakes the carrier handlers.
func TestExpectedAdmissionIndexByCarrierIdentity(t *testing.T) {
	now := time.Now().UTC()
	registry, err := NewExpectedAdmissionRegistry(ExpectedAdmissionRegistryConfig{NodeID: "edge_1", ProcessEpoch: "_epoch12", MaximumAdmissions: 64})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	var admissions []ExpectedAdmission
	for carrier := 0; carrier < 3; carrier++ {
		for route := 2; route >= 0; route-- {
			a := testExpectedAdmissionForRegistry(now.Add(time.Hour))
			a.Identity.SessionID = fmt.Sprintf("session_%d", carrier)
			a.PreviewID, a.OperationID, a.RouteID = fmt.Sprintf("preview_%d_%d", carrier, route), fmt.Sprintf("operation_%d_%d", carrier, route), fmt.Sprintf("route_%d_%d", carrier, route)
			a.Hostname = fmt.Sprintf("p-%d-%d.preview.example.test", carrier, route)
			a.Endpoint = "https://" + a.Hostname
			admissions = append(admissions, a)
		}
	}
	changes := registry.Changed()
	if err := registry.Replace(admissions, now); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changes:
	default:
		t.Fatal("Replace did not close Changed")
	}
	identity := admissions[3].Identity // carrier 1
	got := registry.ForIdentity(identity, now)
	if len(got) != 3 {
		t.Fatalf("carrier 1 has %d admissions, want 3", len(got))
	}
	for i, a := range got {
		if a.Identity != identity || (i > 0 && got[i-1].key() >= a.key()) {
			t.Fatalf("index for carrier 1 = %+v", got)
		}
	}
	// A change for one carrier is visible at once in its lookup.
	if err := registry.Replace(admissions[:3], now); err != nil {
		t.Fatal(err)
	}
	if len(registry.ForIdentity(identity, now)) != 0 || len(registry.ForIdentity(admissions[0].Identity, now)) != 3 {
		t.Fatal("index not rebuilt on change")
	}
}
