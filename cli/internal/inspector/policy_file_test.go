package inspector

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func reloaded(t *testing.T, path string, keep func(string) bool) *Store {
	t.Helper()
	store := NewStore()
	if err := store.RestorePolicies(path, keep); err != nil {
		t.Fatal(err)
	}
	return store
}

// Capture the owner enabled is still on after a runtime restart (a new Store), with its body modes, so the next
// request is captured without anyone re-enabling it; a preview that cannot resume loses its policy.
func TestCapturePolicySurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "preview", "inspector-policies.json")
	keepAll := func(string) bool { return true }
	first := reloaded(t, path, keepAll)
	if err := first.SetPolicy("preview_a", ResourcePolicy{Enabled: true, CaptureRequestBody: true, CaptureResponseBody: true, CaptureRaw: true, ResourceGeneration: 7}); err != nil {
		t.Fatal(err)
	}
	if err := first.SetPolicy("preview_b", ResourcePolicy{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := first.SetPolicy("preview_off", ResourcePolicy{Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("policy file = %v, %v; want owner-only", info, err)
		}
	}

	second := reloaded(t, path, keepAll)
	if !second.Enabled("preview_a") || !second.Enabled("preview_b") || second.Enabled("preview_off") {
		t.Fatal("enabled previews were not restored exactly")
	}
	a := second.policies["preview_a"]
	if !a.CaptureRequestBody || !a.CaptureResponseBody || !a.CaptureRaw || a.ResourceGeneration != 0 {
		t.Fatalf("restored policy = %+v, want bodies and raw on and no stored generation", a)
	}
	if _, err := second.TryBegin("preview_b"); err != nil {
		t.Fatalf("capture after restart: %v", err)
	}

	third := reloaded(t, path, func(id string) bool { return id == "preview_b" })
	if third.Enabled("preview_a") || !third.Enabled("preview_b") {
		t.Fatal("a preview that cannot resume kept its policy")
	}
	if err := third.Forget("preview_b"); err != nil {
		t.Fatal(err)
	}
	if reloaded(t, path, keepAll).Enabled("preview_b") {
		t.Fatal("a forgotten policy came back")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("policy file with no enabled capture: %v", err)
	}
}
