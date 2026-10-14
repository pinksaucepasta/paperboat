//go:build darwin || linux

package localapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
	env "github.com/pinksaucepasta/paperboat/internal/environmente2ee"
)

func vaultCustodyFixture(t *testing.T) (string, []byte) {
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
	defer clear(payload)
	head := env.VaultHead{Issuer: "https://api.example.test", AccountID: "account_1", Generation: 1}
	raw, head, err := env.SealPasswordVault(context.Background(), head, env.DocumentID{}, []byte("test-password"), payload)
	if err != nil {
		t.Fatal(err)
	}
	next, nextHead, err := env.SealPasswordVault(context.Background(), env.VaultHead{Issuer: head.Issuer, AccountID: head.AccountID, Generation: 2}, head.ID, []byte("test-password"), payload)
	if err != nil {
		t.Fatal(err)
	}
	record := config.PasswordVaultRecord{Head: head, Envelope: raw, Payload: payload, Pending: &config.PasswordVaultPending{Head: nextHead, Envelope: next, Payload: payload}, Operation: &config.VaultOperation{WorkspaceID: "personal", Kind: "scope-put", OwnerKind: "personal", OwnerID: head.AccountID, Request: json.RawMessage(`{"encrypted":"opaque-fixture"}`)}}
	encoded, err := json.Marshal(struct {
		Version int `json:"version"`
		config.PasswordVaultRecord
	}{1, record})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(head.Issuer + "\x00" + head.AccountID + "\x00password_vault"))
	t.Cleanup(func() { clear(encoded); clear(raw); clear(next) })
	return vaultCustodyReferencePrefix + hex.EncodeToString(digest[:16]), encoded
}
func decodedCustody(t *testing.T, raw []byte) config.PasswordVaultRecord {
	t.Helper()
	var record struct {
		Version int `json:"version"`
		config.PasswordVaultRecord
	}
	if json.Unmarshal(raw, &record) != nil || record.Version != 1 {
		t.Fatal("invalid custody response")
	}
	return record.PasswordVaultRecord
}
func mustCustody(t *testing.T, client *Client, request VaultCustodyRequest) VaultCustodyResponse {
	t.Helper()
	out, err := client.VaultCustody(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { clear(out.Value) })
	return out
}
func startVaultCustodySocket(t *testing.T, store *VaultCustodyStore) *Client {
	t.Helper()
	root := localAPITestDir(t)
	socket := filepath.Join(root, "vault.sock")
	server, err := NewServer(ServerConfig{SocketPath: socket, OwnerUID: os.Geteuid(), OwnerGID: os.Getegid(), Source: snapshotSourceFunc(func(context.Context) (Snapshot, error) { return validSnapshot(), nil }), VaultCustody: store})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		case <-time.After(time.Second):
			t.Error("custody socket did not stop")
		}
		store.Close()
	})
	waitForSocket(t, socket)
	client, err := NewClient(socket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.http.CloseIdleConnections)
	return client
}
func TestVaultCustodyAuthenticatedSocketSequentialClientsAndLockFence(t *testing.T) {
	ref, raw := vaultCustodyFixture(t)
	store := NewVaultCustodyStore()
	first := startVaultCustodySocket(t, store)
	second, err := NewClient(first.socket, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer second.http.CloseIdleConnections()
	request := VaultCustodyRequest{Reference: ref, Operation: "get", SessionID: "session_1"}
	empty := mustCustody(t, first, request)
	if len(empty.Value) != 0 || empty.Generation == 0 {
		t.Fatal("fresh daemon contained custody")
	}
	begin := mustCustody(t, first, VaultCustodyRequest{Reference: ref, Operation: "begin", SessionID: request.SessionID})
	saved := mustCustody(t, first, VaultCustodyRequest{Reference: ref, Operation: "put", SessionID: request.SessionID, Generation: begin.Generation, Lease: begin.Lease, Value: raw, Unlock: true})
	if saved.ExpiresAt.IsZero() || saved.ExpiresAt.After(time.Now().Add(VaultCustodyLifetime)) {
		t.Fatal("unlock did not establish bounded lifetime")
	}
	mustCustody(t, first, VaultCustodyRequest{Reference: ref, Operation: "end", SessionID: request.SessionID, Lease: begin.Lease})
	read := mustCustody(t, second, request)
	record := decodedCustody(t, read.Value)
	defer record.Clear()
	if len(record.Payload) == 0 || record.Pending == nil || len(record.Pending.Payload) == 0 || read.Lease != "" || !read.ExpiresAt.Equal(saved.ExpiresAt) {
		t.Fatal("second client did not receive same unlocked custody")
	}
	edit := mustCustody(t, second, VaultCustodyRequest{Reference: ref, Operation: "put", SessionID: request.SessionID, Generation: read.Generation, Value: raw})
	if !edit.ExpiresAt.Equal(saved.ExpiresAt) {
		t.Fatal("ordinary save extended unlock lifetime")
	}
	stale := VaultCustodyRequest{Reference: ref, Operation: "put", SessionID: request.SessionID, Generation: read.Generation, Value: raw}
	if _, err := first.VaultCustody(context.Background(), stale); !errors.Is(err, ErrVaultCustodyStale) {
		t.Fatalf("old generation error=%v", err)
	}
	held := mustCustody(t, second, VaultCustodyRequest{Reference: ref, Operation: "begin", SessionID: request.SessionID})
	locked := mustCustody(t, first, VaultCustodyRequest{Reference: ref, Operation: "lock", SessionID: request.SessionID})
	stale.Generation = held.Generation
	stale.Lease = held.Lease
	if _, err := second.VaultCustody(context.Background(), stale); !errors.Is(err, ErrVaultCustodyStale) {
		t.Fatalf("held save crossed explicit lock: %v", err)
	}
	lockedRead := mustCustody(t, first, request)
	lockedRecord := decodedCustody(t, lockedRead.Value)
	defer lockedRecord.Clear()
	if len(lockedRecord.Payload) != 0 || len(lockedRecord.Pending.Payload) != 0 || lockedRecord.Operation == nil || len(lockedRecord.Envelope) == 0 || len(lockedRecord.Pending.Envelope) == 0 || lockedRead.Generation != locked.Generation || !lockedRead.ExpiresAt.IsZero() {
		t.Fatal("lock did not retain only encrypted/reconciliation state")
	}
	stale.Generation = lockedRead.Generation
	stale.Lease = ""
	if _, err := second.VaultCustody(context.Background(), stale); !errors.Is(err, ErrVaultCustodyStale) {
		t.Fatalf("ordinary save resurrected locked keys: %v", err)
	}
	wrongRef := VaultCustodyRequest{Reference: vaultCustodyReferencePrefix + strings.Repeat("a", 32), Operation: "get", SessionID: request.SessionID}
	other := mustCustody(t, first, wrongRef)
	wrongRef.Operation = "put"
	wrongRef.Generation = other.Generation
	wrongRef.Value = raw
	wrongRef.Unlock = true
	if _, err := first.VaultCustody(context.Background(), wrongRef); err == nil {
		t.Fatal("record accepted under unrelated issuer/account reference")
	}
	unlocked := mustCustody(t, first, VaultCustodyRequest{Reference: ref, Operation: "put", SessionID: request.SessionID, Generation: lockedRead.Generation, Value: raw, Unlock: true})
	switched := mustCustody(t, second, VaultCustodyRequest{Reference: ref, Operation: "get", SessionID: "session_2"})
	if len(switched.Value) != 0 || switched.Generation <= unlocked.Generation {
		t.Fatal("session change retained old custody")
	}
	if _, err := first.VaultCustody(context.Background(), VaultCustodyRequest{Reference: ref, Operation: "put", SessionID: request.SessionID, Generation: switched.Generation, Value: raw, Unlock: true}); !errors.Is(err, ErrVaultCustodyStale) {
		t.Fatal("old session changed current session custody")
	}
}
func TestVaultCustodyTimerErasesKeysAndShutdownDropsAllState(t *testing.T) {
	ref, raw := vaultCustodyFixture(t)
	store := newVaultCustodyStore(40*time.Millisecond, time.Second)
	defer store.Close()
	request := VaultCustodyRequest{Reference: ref, Operation: "get", SessionID: "session_1"}
	initial, err := store.Handle(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	unlocked, err := store.Handle(context.Background(), VaultCustodyRequest{Reference: ref, Operation: "put", SessionID: request.SessionID, Generation: initial.Generation, Value: raw, Unlock: true})
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	entry := store.entries[ref]
	heldPayload, heldPending := entry.record.Payload, entry.record.Pending.Payload
	store.mu.Unlock()
	deadline := time.Now().Add(time.Second)
	for {
		store.mu.Lock()
		erased := len(entry.record.Payload) == 0 && len(entry.record.Pending.Payload) == 0 && entry.generation > unlocked.Generation
		zeroed := bytes.Equal(heldPayload, make([]byte, len(heldPayload))) && bytes.Equal(heldPending, make([]byte, len(heldPending)))
		store.mu.Unlock()
		if erased {
			if !zeroed {
				t.Fatal("timer detached keys without zeroing owned buffers")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("keys did not expire without a client request")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := store.Handle(context.Background(), VaultCustodyRequest{Reference: ref, Operation: "put", SessionID: request.SessionID, Generation: unlocked.Generation, Value: raw}); !errors.Is(err, ErrVaultCustodyStale) {
		t.Fatal("save crossed timer expiry")
	}
	current, err := store.Handle(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	clear(current.Value)
	if _, err := store.Handle(context.Background(), VaultCustodyRequest{Reference: ref, Operation: "put", SessionID: request.SessionID, Generation: current.Generation, Value: raw}); !errors.Is(err, ErrVaultCustodyStale) {
		t.Fatal("ordinary current-generation save resurrected expired keys")
	}
	if _, err := store.Handle(context.Background(), VaultCustodyRequest{Reference: ref, Operation: "put", SessionID: request.SessionID, Generation: current.Generation, Value: raw, Unlock: true}); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	envelope := store.entries[ref].record.Envelope
	store.mu.Unlock()
	store.Close()
	if !bytes.Equal(envelope, make([]byte, len(envelope))) {
		t.Fatal("shutdown retained envelope buffers")
	}
	if _, err := store.Handle(context.Background(), request); !errors.Is(err, ErrVaultCustodyStale) {
		t.Fatal("closed daemon returned custody")
	}
	// An initialization which read missing custody before a daemon restart
	// must not recreate it afterward, even without a mutation lease.
	oldDaemon := NewVaultCustodyStore()
	oldMissing, err := oldDaemon.Handle(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	oldDaemon.Close()
	fresh := NewVaultCustodyStore()
	defer fresh.Close()
	empty, err := fresh.Handle(context.Background(), request)
	if err != nil || len(empty.Value) != 0 {
		t.Fatal("restarted daemon retained custody")
	}
	if _, err := fresh.Handle(context.Background(), VaultCustodyRequest{Reference: ref, Operation: "put", SessionID: request.SessionID, Generation: oldMissing.Generation, Value: raw, Unlock: true}); !errors.Is(err, ErrVaultCustodyStale) {
		t.Fatal("pre-restart initialization resurrected daemon custody")
	}

}
func TestVaultCustodyLeaseCancellationExpiryAndBounds(t *testing.T) {
	store := newVaultCustodyStore(time.Second, 40*time.Millisecond)
	defer store.Close()
	ref := vaultCustodyReferencePrefix + strings.Repeat("a", 32)
	begin, err := store.Handle(context.Background(), VaultCustodyRequest{Reference: ref, Operation: "begin", SessionID: "session_1"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Handle(ctx, VaultCustodyRequest{Reference: ref, Operation: "begin", SessionID: "session_1"}); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled lease waiter succeeded")
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	next, err := store.Handle(ctx, VaultCustodyRequest{Reference: ref, Operation: "begin", SessionID: "session_1"})
	if err != nil || next.Lease == begin.Lease || next.Generation <= begin.Generation {
		t.Fatal("abandoned lease did not expire and fence")
	}
	if _, err := store.Handle(context.Background(), VaultCustodyRequest{Reference: ref, Operation: "end", SessionID: "session_1", Lease: begin.Lease}); !errors.Is(err, ErrVaultCustodyStale) {
		t.Fatal("old lease released current transaction")
	}
	for _, suffix := range []string{"b", "c", "d"} {
		if _, err := store.Handle(context.Background(), VaultCustodyRequest{Reference: vaultCustodyReferencePrefix + strings.Repeat(suffix, 32), Operation: "get", SessionID: "session_1"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Handle(context.Background(), VaultCustodyRequest{Reference: vaultCustodyReferencePrefix + strings.Repeat("e", 32), Operation: "get", SessionID: "session_1"}); !errors.Is(err, ErrVaultCustodyLimit) {
		t.Fatal("unbounded custody reference allocation")
	}
	oversized := make([]byte, vaultCustodyMaximumValueBytes+1)
	defer clear(oversized)
	if _, err := store.Handle(context.Background(), VaultCustodyRequest{Reference: ref, Operation: "put", SessionID: "session_1", Value: oversized}); !errors.Is(err, ErrVaultCustodyInvalid) {
		t.Fatal("oversized request accepted")
	}
}
func TestVaultCustodyRequiresExactOwnerBeforeReadingSecretBody(t *testing.T) {
	store := NewVaultCustodyStore()
	defer store.Close()
	for _, tc := range []struct {
		sid  string
		peer Peer
	}{{"", Peer{UID: os.Geteuid() + 1}}, {"S-1-5-21-owner", Peer{SID: "S-1-5-18"}}} {
		server := &Server{config: ServerConfig{OwnerUID: os.Geteuid(), OwnerSID: tc.sid, Authorize: func(Peer) bool { return true }, VaultCustody: store}}
		secret := "private-marker-must-not-appear"
		request := httptest.NewRequest(http.MethodPost, "/v1/vault-custody", strings.NewReader(secret))
		request = request.WithContext(context.WithValue(request.Context(), peerContextKey{}, tc.peer))
		response := httptest.NewRecorder()
		server.handler().ServeHTTP(response, request)
		if response.Code != http.StatusForbidden || strings.Contains(response.Body.String(), secret) || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("custody endpoint admitted nonowner or disclosed request payload")
		}
	}
}
