//go:build linux

package machineguard

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLegacyLoopbackStateTrimsPersistedNewline(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, loopbackCIDRFile), []byte("127.212.0.0/16\n"), 0600); err != nil {
		t.Fatal(err)
	}
	state, err := loadLoopbackState(dir)
	if err != nil || state.Active != "127.212.0.0/16" {
		t.Fatalf("state=%+v err=%v", state, err)
	}
}

func TestCorruptLoopbackStateFailsClosed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, loopbackStateFile), []byte(`{"active":"127.0.0.0/16"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadLoopbackState(dir); err == nil {
		t.Fatal("corrupt protected range state accepted")
	}
}

func TestLoopbackStateMigratesHistoricalRangeToFixedActiveRange(t *testing.T) {
	dir := t.TempDir()
	old := loopbackState{Active: "127.212.0.0/16", Protected: []string{"127.212.0.0/16"}}
	if err := writeLoopbackState(dir, old); err != nil {
		t.Fatal(err)
	}
	changedState, err := mergedLoopbackState(old, "127.100.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeLoopbackState(dir, changedState); err != nil {
		t.Fatal(err)
	}
	changed, err := loadLoopbackState(dir)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Active != "127.100.0.0/16" || !reflect.DeepEqual(changed.Protected, []string{"127.100.0.0/16", "127.212.0.0/16"}) {
		t.Fatalf("protected=%v", changed.Protected)
	}
	if err = storeLoopbackCIDR(dir, "127.212.0.0/16"); err == nil {
		t.Fatal("custom active range remained configurable")
	}
}

func TestRangeChangeRejectsCustomRangeAndOldAddresses(t *testing.T) {
	owner := &testControlConn{identity: "0"}
	g := &guardServer{cfg: Config{LoopbackCIDR: "127.100.0.0/16"}, leases: map[string]*guardedLease{}, connections: map[controlConn]string{owner: "0"}}
	if _, err := g.handle(context.Background(), owner, "0", request{Operation: "prepare_range", LoopbackCIDR: "127.212.0.0/16"}); err == nil {
		t.Fatal("custom machine range change accepted")
	}
	if _, err := g.handle(context.Background(), owner, "0", request{Operation: "acquire", Hostname: "office.local.pprbt.dev", IP: "127.212.23.45", Port: 8080}); err == nil {
		t.Fatal("historical custom range accepted for active forwarding")
	}
}
