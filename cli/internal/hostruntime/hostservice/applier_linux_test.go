package hostservice

import (
	"context"
	"path/filepath"
	"testing"
)

func TestKeepAwakeWithoutLogindIsAlreadyApplied(t *testing.T) {
	previous := systemBus
	systemBus = filepath.Join(t.TempDir(), "missing")
	t.Cleanup(func() { systemBus = previous })
	applier := &platformApplier{}
	if err := applier.Apply(context.Background(), KeepAwake); err != nil {
		t.Fatalf("keep awake without logind: %v", err)
	}
	if applier.command != nil {
		t.Fatal("started an inhibitor without logind")
	}
}
