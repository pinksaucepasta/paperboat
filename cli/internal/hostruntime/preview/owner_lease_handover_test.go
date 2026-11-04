package preview

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pbvm4 feat8: foreground previews paused ~9 s at pb update until the CLI
// reattached. The next generation must hold the lease, and pass its owner
// session to the preview restore, before the CLI talks to it.
func TestForegroundLeaseIsTakenOverByTheNextGeneration(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	dir := filepath.Join(t.TempDir(), "foreground-leases")
	generation := func() (*OwnerSessionLeaseManager, *RuntimeOwnerSessionRegistry) {
		registry, err := NewRuntimeOwnerSessionRegistry(RuntimeOwnerSessionRegistryConfig{MachineID: "machine_01", RuntimeDone: make(chan struct{})})
		if err != nil {
			t.Fatal(err)
		}
		manager, err := NewOwnerSessionLeaseManager(OwnerSessionLeaseManagerConfig{MachineID: "machine_01", ControlToken: "control_secret", Registry: registry, Now: func() time.Time { return now }, HandoverDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		return manager, registry
	}
	old, _ := generation()
	lease, err := old.Acquire(OwnerSessionLeaseRequest{Target: LeaseTarget{Scheme: "http", Address: "127.0.0.1:3000"}}, "foreground_01")
	if err != nil {
		t.Fatal(err)
	}
	record, err := os.ReadFile(filepath.Join(dir, lease.ID+".json"))
	if err != nil || strings.Contains(string(record), lease.Token) {
		t.Fatalf("handover record missing or holds the raw token: %v", err)
	}
	next, registry := generation()
	if !registry.LocalOwnerAcquired(lease.OwnerSessionID) {
		t.Fatal("next generation does not hold the owner session before the CLI reattaches")
	}
	if _, err := next.Heartbeat(lease.ID, "wrong-token"); !errors.Is(err, ErrOwnerSessionLeaseUnauthorized) {
		t.Fatalf("wrong token = %v", err)
	}
	// The CLI keeps heartbeating the old generation for a while; the next one
	// must not expire the lease the old one renewed.
	now = now.Add(10 * time.Second)
	if _, err := old.Heartbeat(lease.ID, lease.Token); err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Second)
	next.Sweep(now)
	renewed, err := next.Heartbeat(lease.ID, lease.Token)
	if err != nil || renewed.Token != lease.Token {
		t.Fatalf("taken-over heartbeat = %+v, %v", renewed, err)
	}
	if err := next.Release(lease.ID, lease.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, lease.ID+".json")); !os.IsNotExist(err) {
		t.Fatalf("released lease still handed over: %v", err)
	}
	if third, registry := generation(); registry.LocalOwnerAcquired(lease.OwnerSessionID) || len(third.leases) != 0 {
		t.Fatal("released lease came back in a later generation")
	}
}

func TestExpiredForegroundLeaseIsNotTakenOver(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	registry, _ := NewRuntimeOwnerSessionRegistry(RuntimeOwnerSessionRegistryConfig{MachineID: "machine_01", RuntimeDone: make(chan struct{})})
	old, err := NewOwnerSessionLeaseManager(OwnerSessionLeaseManagerConfig{MachineID: "machine_01", ControlToken: "control_secret", Registry: registry, Now: func() time.Time { return now }, HandoverDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := old.Acquire(OwnerSessionLeaseRequest{Target: LeaseTarget{Scheme: "http", Address: "127.0.0.1:3000"}}, "foreground_01")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	next, err := NewOwnerSessionLeaseManager(OwnerSessionLeaseManagerConfig{MachineID: "machine_01", ControlToken: "control_secret", Registry: registry, Now: func() time.Time { return now }, HandoverDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := next.Heartbeat(lease.ID, lease.Token); !errors.Is(err, ErrOwnerSessionLeaseUnauthorized) {
		t.Fatalf("expired lease taken over: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, lease.ID+".json")); !os.IsNotExist(err) {
		t.Fatal("expired record kept")
	}
}
