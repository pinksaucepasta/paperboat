//go:build darwin || linux

package localdaemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
	env "github.com/pinksaucepasta/paperboat/internal/environmente2ee"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

func unlockedVaultRecord(t *testing.T) (string, []byte) {
	t.Helper()
	keys, err := env.NewVaultKeys()
	if err != nil {
		t.Fatal(err)
	}
	defer keys.Clear()
	payload, err := keys.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	head := env.VaultHead{Issuer: "https://api.example.test", AccountID: "account_1", Generation: 1}
	sealed, head, err := env.SealPasswordVault(context.Background(), head, env.DocumentID{}, []byte("test-password"), payload)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(struct {
		Version int `json:"version"`
		config.PasswordVaultRecord
	}{1, config.PasswordVaultRecord{Head: head, Envelope: sealed, Payload: payload}})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(head.Issuer + "\x00" + head.AccountID + "\x00password_vault"))
	return "environment-password-vault-v1-" + hex.EncodeToString(digest[:16]), raw
}

// serveCustody runs one daemon's local API (custody only) on socket until the returned stop.
func serveCustody(t *testing.T, socket string, store *localapi.VaultCustodyStore) func() {
	t.Helper()
	server, err := localapi.NewServer(localapi.ServerConfig{SocketPath: socket, OwnerUID: os.Geteuid(), OwnerGID: os.Getegid(), Source: emptySnapshotSource{}, VaultCustody: store})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(socket); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatal("custody socket did not start")
		}
	}
	return func() {
		cancel()
		<-done
		_ = store.Close()
	}
}

type emptySnapshotSource struct{}

func (emptySnapshotSource) Snapshot(context.Context) (localapi.Snapshot, error) {
	return localapi.Snapshot{}, errors.New("no snapshot in this test")
}

func handoffFixture(t *testing.T) (socket string, daemon func(context.Context) (*localapi.Client, error)) {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "pb-vh-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	socket = filepath.Join(root, "api.sock")
	daemon = func(context.Context) (*localapi.Client, error) {
		return localapi.NewOwnerClient(socket, time.Second, os.Geteuid())
	}
	return socket, daemon
}

func custodyValue(t *testing.T, client *localapi.Client, ref string) []byte {
	t.Helper()
	out, err := client.VaultCustody(context.Background(), localapi.VaultCustodyRequest{Reference: ref, Operation: "get", SessionID: "session_1"})
	if err != nil {
		t.Fatal(err)
	}
	return out.Value
}

func TestHandOverVaultCustodySurvivesDaemonReplacement(t *testing.T) {
	socket, daemon := handoffFixture(t)
	ref, raw := unlockedVaultRecord(t)
	stopOld := serveCustody(t, socket, localapi.NewVaultCustodyStore())
	client, _ := daemon(context.Background())
	begin, err := client.VaultCustody(context.Background(), localapi.VaultCustodyRequest{Reference: ref, Operation: "begin", SessionID: "session_1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.VaultCustody(context.Background(), localapi.VaultCustodyRequest{Reference: ref, Operation: "put", SessionID: "session_1", Generation: begin.Generation, Lease: begin.Lease, Value: raw, Unlock: true}); err != nil {
		t.Fatal(err)
	}
	var stopNew func()
	err = handOverVaultCustody(context.Background(), daemon, func() error {
		stopOld()
		stopNew = serveCustody(t, socket, localapi.NewVaultCustodyStore())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stopNew()
	client, _ = daemon(context.Background())
	if len(custodyValue(t, client, ref)) == 0 {
		t.Fatal("new daemon is locked after the replacement")
	}
}

// A handoff that cannot reach the new daemon leaves it locked (pb env unlock) and does not fail the
// replacement; a failed replacement still reports its own error.
func TestHandOverVaultCustodyFailureLeavesNewDaemonLocked(t *testing.T) {
	socket, daemon := handoffFixture(t)
	ref, raw := unlockedVaultRecord(t)
	stopOld := serveCustody(t, socket, localapi.NewVaultCustodyStore())
	client, _ := daemon(context.Background())
	if _, err := client.VaultCustody(context.Background(), localapi.VaultCustodyRequest{Reference: ref, Operation: "get", SessionID: "session_1"}); err != nil {
		t.Fatal(err)
	}
	begin, _ := client.VaultCustody(context.Background(), localapi.VaultCustodyRequest{Reference: ref, Operation: "begin", SessionID: "session_1"})
	if _, err := client.VaultCustody(context.Background(), localapi.VaultCustodyRequest{Reference: ref, Operation: "put", SessionID: "session_1", Generation: begin.Generation, Lease: begin.Lease, Value: raw, Unlock: true}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := handOverVaultCustody(ctx, daemon, func() error { stopOld(); return nil }); err != nil {
		t.Fatalf("an unreachable new daemon failed the replacement: %v", err)
	}
	stopNew := serveCustody(t, socket, localapi.NewVaultCustodyStore())
	defer stopNew()
	client, _ = daemon(context.Background())
	if len(custodyValue(t, client, ref)) != 0 {
		t.Fatal("new daemon holds custody after a failed handoff")
	}
	restartErr := errors.New("restart failed")
	if err := handOverVaultCustody(context.Background(), daemon, func() error { return restartErr }); !errors.Is(err, restartErr) {
		t.Fatalf("restart error=%v", err)
	}
}
