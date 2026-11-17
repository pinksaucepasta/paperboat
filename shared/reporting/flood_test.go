package reporting

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testGuard(path string, clock *time.Time) *FloodGuard {
	g := NewFloodGuard(path)
	g.now = func() time.Time { return *clock }
	return g
}

func TestFloodGuardSendsBurstThenOnePerHourWithSuppressedCount(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	g := testGuard("", &now)
	for i := 0; i < floodBurst; i++ {
		if ok, n := g.Admit("a"); !ok || n != 0 {
			t.Fatalf("burst %d: ok=%v suppressed=%d", i, ok, n)
		}
	}
	for i := 0; i < 5; i++ {
		now = now.Add(5 * time.Second) // a daemon restarted every 5 s
		if ok, _ := g.Admit("a"); ok {
			t.Fatalf("repeat %d passed", i)
		}
	}
	if ok, _ := g.Admit("b"); !ok {
		t.Fatal("another fault was held back by the first")
	}
	now = now.Add(time.Hour)
	if ok, n := g.Admit("a"); !ok || n != 5 {
		t.Fatalf("after an hour: ok=%v suppressed=%d, want true 5", ok, n)
	}
	if ok, n := g.Admit("a"); ok || n != 0 {
		t.Fatalf("second in the same hour passed: %v %d", ok, n)
	}
}

func TestFloodGuardHourlyBudgetCoversAllFaults(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	g := testGuard("", &now)
	sent := 0
	for i := 0; i < 200; i++ {
		if ok, _ := g.Admit(string(rune('A' + i%50))); ok {
			sent++
		}
	}
	if sent != floodBudget {
		t.Fatalf("sent %d in one hour, want the budget %d", sent, floodBudget)
	}
}

func TestFloodGuardStateSurvivesRestartAndBadFiles(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	path := filepath.Join(t.TempDir(), "flood.json")
	sent := 0
	for process := 0; process < 20; process++ { // a crash-loop: a new process each time
		if ok, _ := testGuard(path, &now).Admit("loop"); ok {
			sent++
		}
		now = now.Add(5 * time.Second)
	}
	if sent != floodBurst {
		t.Fatalf("crash-loop sent %d, want %d", sent, floodBurst)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state file: %v %v", info, err)
	}
	for _, bad := range []string{"not json", `{"keys":{"x":null}}`} {
		if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		if ok, _ := testGuard(path, &now).Admit("fresh"); !ok {
			t.Fatalf("corrupt state %q blocked reporting", bad)
		}
	}
	// An unwritable location (read-only file system) only costs persistence.
	g := testGuard(filepath.Join(t.TempDir(), "missing", "flood.json"), &now)
	for i := 0; i < floodBurst; i++ {
		if ok, _ := g.Admit("ro"); !ok {
			t.Fatal("unwritable state path blocked reporting")
		}
	}
	if ok, _ := g.Admit("ro"); ok {
		t.Fatal("in-memory limit lost when the file cannot be written")
	}
}

func TestFloodGuardRemembersBoundedKeys(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	g := testGuard("", &now)
	for i := 0; i < 3*floodKeys; i++ {
		now = now.Add(time.Second)
		g.Admit(string(rune(0x100 + i)))
	}
	if len(g.state.Keys) > floodKeys {
		t.Fatalf("%d keys kept", len(g.state.Keys))
	}
	var nilGuard *FloodGuard
	if ok, _ := nilGuard.Admit("x"); !ok {
		t.Fatal("nil guard must admit")
	}
}
