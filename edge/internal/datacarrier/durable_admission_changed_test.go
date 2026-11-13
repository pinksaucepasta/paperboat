package datacarrier

import (
	"testing"
	"time"
)

func TestDurableAdmissionRegistryChangedClosesOnReplace(t *testing.T) {
	r, err := NewDurableAdmissionRegistry(DurableAdmissionRegistryConfig{NodeID: "edge_changed", ProcessEpoch: "_edge_changed_1"})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	first := r.Changed()
	select {
	case <-first:
		t.Fatal("Changed closed before any Replace")
	default:
	}
	if err := r.Replace(nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-first:
	default:
		t.Fatal("Changed not closed by Replace")
	}
	if second := r.Changed(); second == first {
		t.Fatal("Changed reused a closed channel")
	}
}
