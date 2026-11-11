//go:build linux || darwin || windows

package hostinstall

import (
	"context"
	"errors"
	"testing"
)

// A generation that cannot be removed does not stop the removal of the others, and every failure is reported.
func TestRemoveOtherCarrierGenerationsTriesAllAndKeepsTheBaseline(t *testing.T) {
	generations := []CarrierConfig{{Generation: "a"}, {Generation: "b"}, {Generation: "c"}, {Generation: "d"}}
	failB, failD := errors.New("b is held"), errors.New("d is held")
	var removed []string
	err := removeOtherCarrierGenerations(context.Background(), generations, func(c CarrierConfig) bool { return c.Generation == "a" },
		func(_ context.Context, c CarrierConfig) error {
			removed = append(removed, c.Generation)
			switch c.Generation {
			case "b":
				return failB
			case "d":
				return failD
			}
			return nil
		})
	if !errors.Is(err, failB) || !errors.Is(err, failD) {
		t.Fatalf("failures not reported together: %v", err)
	}
	if len(removed) != 3 || removed[0] != "b" || removed[1] != "c" || removed[2] != "d" {
		t.Fatalf("removed %v, want b c d (the baseline a is kept, a failure does not stop the rest)", removed)
	}
}
