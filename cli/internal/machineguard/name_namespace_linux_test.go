package machineguard

import "testing"

// F23-01: with the gateway routed by uid, machine names are per OS user. Two users whose accounts each have a
// machine called hp both get hp.<domain>: the second is not refused because the first holds the name, and a
// reservation journal of an earlier, host-wide guard moves each name into its owner's namespace.
func TestTwoOSUsersMayUseTheSameMachineName(t *testing.T) {
	g := &guardServer{reserved: reservations{Names: map[string]string{}}, leases: map[string]*guardedLease{}}
	name := "hp." + testBrowserDomain
	for _, uid := range []string{"1000", "1001", "1000"} {
		if err := g.reserveName(uid, name); err != nil {
			t.Fatalf("uid %s was refused its own machine name: %v", uid, err)
		}
	}
	if g.reserved.Names[nameKey("1001", name)] != "1001" || g.reserved.Names[nameKey("1000", name)] != "1000" || len(g.reserved.Names) != 2 {
		t.Fatalf("reservations=%v, want one per OS user", g.reserved.Names)
	}
	legacy := map[string]string{name: "1000", "dev." + testBrowserDomain: "1001"}
	scopeNameReservations(legacy)
	if legacy["1000:"+name] != "1000" || legacy["1001:dev."+testBrowserDomain] != "1001" || len(legacy) != 2 {
		t.Fatalf("legacy reservations=%v", legacy)
	}
}
