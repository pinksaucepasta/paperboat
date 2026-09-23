//go:build linux

package deviceguard

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

func TestLoopbackStateRetainsPriorRangesAndCanRollbackExactly(t *testing.T) {
	dir := t.TempDir()
	old := loopbackState{Active: "127.100.0.0/16", Protected: []string{"127.100.0.0/16"}}
	if err := writeLoopbackState(dir, old); err != nil {
		t.Fatal(err)
	}
	changedState, err := mergedLoopbackState(old, "127.212.0.0/16")
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
	if !reflect.DeepEqual(changed.Protected, []string{"127.212.0.0/16", "127.100.0.0/16"}) {
		t.Fatalf("protected=%v", changed.Protected)
	}
	if err := writeLoopbackState(dir, old); err != nil {
		t.Fatal(err)
	}
	restored, err := loadLoopbackState(dir)
	if err != nil || !reflect.DeepEqual(restored, old) {
		t.Fatalf("restored=%+v err=%v", restored, err)
	}
}

func TestRangeChangeFenceRejectsOtherConnectionsAndAdmissions(t *testing.T) {
	owner := &certificateConnection{identity: "0"}
	other := &certificateConnection{identity: "1000"}
	g := &guardServer{cfg: Config{LoopbackCIDR: "127.100.0.0/16"}, leases: map[string]*guardedLease{}, connections: map[controlConn]string{owner: "0", other: "1000"}}
	if _, err := g.handle(context.Background(), owner, "0", request{Operation: "prepare_range", LoopbackCIDR: "127.212.0.0/16"}); err == nil {
		t.Fatal("range change accepted with another connected local owner")
	}
	delete(g.connections, other)
	if _, err := g.handle(context.Background(), owner, "0", request{Operation: "prepare_range", LoopbackCIDR: "127.212.0.0/16"}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.handle(context.Background(), other, "1000", request{Operation: "acquire", Hostname: "office.pprbt", IP: "127.100.23.45", Port: 8080}); err == nil {
		t.Fatal("listener admitted during fenced range change")
	}
}
