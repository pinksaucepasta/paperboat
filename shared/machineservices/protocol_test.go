package machineservices

import (
	"strings"
	"testing"
	"time"
)

func TestSnapshotBoundsAndExactPorts(t *testing.T) {
	now := time.Now().UTC()
	good := Snapshot{Schema: "paperboat.machine-services/v1", InstallationGeneration: 1, BootID: "boot-012345678901", StartedAt: now, Generation: 1, Services: []Port{{5432, "ipv4"}}}
	if e := ValidateSnapshot(good, now); e != nil {
		t.Fatal(e)
	}
	for name, change := range map[string]func(*Snapshot){"duplicate": func(s *Snapshot) { s.Services = append(s.Services, Port{5432, "ipv6"}) }, "overflow": func(s *Snapshot) { s.Services = make([]Port, 65) }, "wildcard family": func(s *Snapshot) { s.Services = []Port{{5432, "0.0.0.0"}} }, "zero port": func(s *Snapshot) { s.Services = []Port{{0, "ipv4"}} }, "counter overflow": func(s *Snapshot) { s.Generation = 9007199254740992 }, "future boot": func(s *Snapshot) { s.StartedAt = now.Add(6 * time.Second) }, "invalid boot": func(s *Snapshot) { s.BootID = strings.Repeat("x", 65) }} {
		t.Run(name, func(t *testing.T) {
			s := good
			change(&s)
			if ValidateSnapshot(s, now) == nil {
				t.Fatal("accepted invalid snapshot")
			}
		})
	}
	for _, route := range []string{"tcp:0", "tcp:65536", "tcp:05432", "tcp:+5432", "tcp: 5432", "http:5432"} {
		if _, e := ParseRoute(route); e == nil {
			t.Fatalf("accepted %q", route)
		}
	}
	if n, e := ParseRoute("tcp:5432"); e != nil || n != 5432 {
		t.Fatalf("route %d %v", n, e)
	}
}
