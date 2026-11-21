//go:build linux || darwin || windows

package machineguard

import (
	"os"
	"path/filepath"
	"testing"
)

// Every acquire persists under the guard's lock; an unchanged state must not be rewritten (two fsyncs each on
// slow storage held every guard request past its 5 s deadline).
func TestPersistSkipsUnchangedReservations(t *testing.T) {
	g := &guardServer{cfg: Config{StateDir: testStateDir(t)}, reserved: reservations{Names: map[string]string{}}}
	path := filepath.Join(g.cfg.StateDir, "reservations.json")
	if err := g.persist(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := g.persist(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("unchanged reservations were written again: %v", err)
	}
	g.reserved.Names["laptop"] = "1000"
	if err := g.persist(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("changed reservations were not written: %v", err)
	}
}
