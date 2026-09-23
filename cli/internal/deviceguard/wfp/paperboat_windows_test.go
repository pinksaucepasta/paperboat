//go:build windows

package wfp

import "testing"

func TestOwnedFilterNamesCoverBoundedLoopbackRanges(t *testing.T) {
	names := ownedFilterNames()
	if len(names) != 5+2*254 {
		t.Fatalf("owned filter names=%d", len(names))
	}
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if seen[name] {
			t.Fatalf("duplicate owned filter name %q", name)
		}
		seen[name] = true
	}
	for _, name := range []string{"paperboat-in-block-1", "paperboat-out-block-1", "paperboat-in-block-100", "paperboat-out-block-123", "paperboat-in-block-254", "paperboat-out-block-254"} {
		if !seen[name] {
			t.Fatalf("missing owned range filter %q", name)
		}
	}
	if seen["paperboat-in-block-0"] || seen["paperboat-out-block-255"] {
		t.Fatal("ordinary or reserved loopback range included in cleanup")
	}
}
