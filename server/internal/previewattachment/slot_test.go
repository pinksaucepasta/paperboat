package previewattachment

import "testing"

func TestSplitOperationSlot(t *testing.T) {
	for operation, want := range map[string]struct {
		base string
		slot int
	}{
		"op_0123": {"op_0123", 0}, "op_0123.b": {"op_0123", 1}, ".b": {".b", 0}, "op.b.b": {"op.b", 1},
	} {
		if base, slot := SplitOperationSlot(operation); base != want.base || slot != want.slot {
			t.Errorf("SplitOperationSlot(%q) = %q, %d; want %q, %d", operation, base, slot, want.base, want.slot)
		}
	}
}
