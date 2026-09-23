package environmentmanager

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/environmente2ee"
)

type vaultControl struct {
	store           config.ProfileStore
	state           api.PasswordVaultState
	failAfterCommit bool
}

func (c *vaultControl) GetPasswordVault(context.Context) (api.PasswordVaultState, error) {
	if c.state.DocumentID == "" {
		return api.PasswordVaultState{}, &api.APIError{Status: 404}
	}
	return c.state, nil
}
func (c *vaultControl) PutPasswordVault(_ context.Context, raw []byte) (api.PasswordVaultState, error) {
	// Publication must be durable locally before a network write is attempted.
	local, err := c.store.LoadPasswordVault("https://control.example", "account_1")
	if err != nil {
		return api.PasswordVaultState{}, err
	}
	defer local.Clear()
	if local.Pending == nil || !bytes.Equal(raw, local.Pending.Envelope) {
		return api.PasswordVaultState{}, errors.New("publication was not durably staged")
	}
	head := local.Pending.Head
	c.state = api.PasswordVaultState{Issuer: head.Issuer, AccountID: head.AccountID, Generation: head.Generation, DocumentID: head.ID.String(), Envelope: base64.RawURLEncoding.EncodeToString(raw)}
	if c.failAfterCommit {
		c.failAfterCommit = false
		return api.PasswordVaultState{}, errors.New("connection lost after commit")
	}
	return c.state, nil
}

func TestPasswordVaultOfflineDeviceAndInterruptedRewrap(t *testing.T) {
	ctx := context.Background()
	store := config.ProfileStore{Path: filepath.Join(t.TempDir(), "profiles.json"), Secrets: &secureMemoryStore{}}
	control := &vaultControl{store: store}
	a := PasswordVault{Client: control, Store: store, Issuer: "https://control.example", AccountID: "account_1"}
	password := []byte("first test master password")
	if err := a.Initialize(ctx, password); err != nil {
		t.Fatal(err)
	}
	initial, err := store.LoadPasswordVault(a.Issuer, a.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	defer initial.Clear()
	if len(initial.Payload) == 0 || initial.Pending != nil {
		t.Fatal("initial custody not committed")
	}
	// B has no A identity, session key, root or local ENV record.
	b := a
	b.Store = config.ProfileStore{Path: filepath.Join(t.TempDir(), "profiles.json"), Secrets: &secureMemoryStore{}}
	if err := b.Unlock(ctx, []byte("wrong")); !errors.Is(err, environmente2ee.ErrVaultUnlock) {
		t.Fatal("wrong password accepted")
	}
	if _, err := b.Store.LoadPasswordVault(b.Issuer, b.AccountID); !errors.Is(err, config.ErrSecretNotFound) {
		t.Fatal("wrong password created custody")
	}
	if err := b.Unlock(ctx, password); err != nil {
		t.Fatal(err)
	}
	bKeys, err := b.Store.LoadPasswordVault(b.Issuer, b.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(initial.Payload, bKeys.Payload) {
		t.Fatal("offline-device unlock changed key material")
	}
	bKeys.Clear()
	control.failAfterCommit = true
	newPassword := []byte("second test master password")
	if err := a.ChangePassword(ctx, newPassword); err == nil {
		t.Fatal("lost response not surfaced")
	}
	pending, err := store.LoadPasswordVault(a.Issuer, a.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Pending == nil || pending.Head != initial.Head || !bytes.Equal(pending.Payload, initial.Payload) {
		t.Fatal("interruption destroyed usable committed keys")
	}
	pending.Clear()
	if err := a.ChangePassword(ctx, []byte("different")); !errors.Is(err, ErrVaultPending) {
		t.Fatal("pending operation silently used a different password")
	}
	if err := a.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.LockPasswordVault(a.Issuer, a.AccountID); err != nil {
		t.Fatal(err)
	}
	if err := a.Unlock(ctx, password); !errors.Is(err, environmente2ee.ErrVaultUnlock) {
		t.Fatal("previous password unlocked successor")
	}
	if err := a.Unlock(ctx, newPassword); err != nil {
		t.Fatal(err)
	}
	current, err := store.LoadPasswordVault(a.Issuer, a.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	defer current.Clear()
	if !bytes.Equal(current.Payload, initial.Payload) || current.Head.Generation != 2 || current.Pending != nil {
		t.Fatal("rewrap did not preserve key inventory")
	}
	control.state = api.PasswordVaultState{Issuer: initial.Head.Issuer, AccountID: initial.Head.AccountID, Generation: 1, DocumentID: initial.Head.ID.String(), Envelope: base64.RawURLEncoding.EncodeToString(initial.Envelope)}
	if err := a.Unlock(ctx, password); !errors.Is(err, ErrAuthorityFork) {
		t.Fatal("rollback accepted")
	}
}

func TestRecoveryKeepsTeamKeysCurrentAndResumesExactSuccessor(t *testing.T) {
	ctx := context.Background()
	store := config.ProfileStore{Path: filepath.Join(t.TempDir(), "profiles.json"), Secrets: &secureMemoryStore{}}
	control := &vaultControl{store: store}
	a := PasswordVault{Client: control, Store: store, Issuer: "https://control.example", AccountID: "account_1"}
	code, err := environmente2ee.GenerateVaultRecoveryCode()
	if err != nil {
		t.Fatal(err)
	}
	defer clear(code)
	if err := a.InitializeWithRecovery(ctx, []byte("first master"), code); err != nil {
		t.Fatal(err)
	}
	teamKey := bytes.Repeat([]byte{0x37}, 32)
	if err := a.UpdateKeys(ctx, func(keys *environmente2ee.VaultKeys) error {
		keys.Teams = []environmente2ee.VaultTeamKey{{TeamID: "team_1", Epoch: 2, MembershipGeneration: 1, Key: bytes.Clone(teamKey)}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, err := store.LoadPasswordVault(a.Issuer, a.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	defer before.Clear()
	b := a
	b.Store = config.ProfileStore{Path: filepath.Join(t.TempDir(), "profiles.json"), Secrets: &secureMemoryStore{}}
	control.store = b.Store
	replacement, err := environmente2ee.GenerateVaultRecoveryCode()
	if err != nil {
		t.Fatal(err)
	}
	defer clear(replacement)
	if err := b.Recover(ctx, []byte("invalid-code"), []byte("new master"), replacement); !errors.Is(err, environmente2ee.ErrVaultUnlock) {
		t.Fatal("malformed recovery accepted")
	}
	if _, err := b.Store.LoadPasswordVault(b.Issuer, b.AccountID); !errors.Is(err, config.ErrSecretNotFound) {
		t.Fatal("bad recovery mutated custody")
	}
	control.failAfterCommit = true
	if err := b.Recover(ctx, code, []byte("new master"), replacement); err == nil {
		t.Fatal("lost response not reported")
	}
	pending, err := b.Store.LoadPasswordVault(b.Issuer, b.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	defer pending.Clear()
	if pending.Pending == nil {
		t.Fatal("recovery successor not staged")
	}
	pendingID := pending.Pending.Head.ID
	for _, value := range b.Store.Secrets.(*secureMemoryStore).values {
		if bytes.Contains([]byte(value), code) || bytes.Contains([]byte(value), replacement) {
			t.Fatal("recovery code persisted")
		}
	}
	if err := b.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := b.Store.LoadPasswordVault(b.Issuer, b.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	defer after.Clear()
	if after.Head.ID != pendingID || !bytes.Equal(after.Payload, before.Payload) {
		t.Fatal("recovery changed keys or retry bytes")
	}
	keys, err := environmente2ee.ParseVaultKeys(after.Payload)
	if err != nil {
		t.Fatal(err)
	}
	defer keys.Clear()
	if len(keys.Teams) != 1 || !bytes.Equal(keys.Teams[0].Key, teamKey) {
		t.Fatal("latest granted team key lost")
	}
	if _, err := environmente2ee.OpenRecoveryVault(ctx, after.Head, code, after.Envelope); !errors.Is(err, environmente2ee.ErrVaultUnlock) {
		t.Fatal("superseded recovery code accepted")
	}
	opened, err := environmente2ee.OpenRecoveryVault(ctx, after.Head, replacement, after.Envelope)
	if err != nil {
		t.Fatal(err)
	}
	clear(opened)
	if err := b.ReplaceRecovery(ctx, nil); err != nil {
		t.Fatal(err)
	}
	disabled, err := b.Store.LoadPasswordVault(b.Issuer, b.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	defer disabled.Clear()
	if _, err := environmente2ee.OpenRecoveryVault(ctx, disabled.Head, replacement, disabled.Envelope); !errors.Is(err, environmente2ee.ErrVaultUnlock) {
		t.Fatal("disabled recovery accepted")
	}
	if err := b.Store.LockPasswordVault(b.Issuer, b.AccountID); err != nil {
		t.Fatal(err)
	}
	if err := b.Unlock(ctx, []byte("new master")); err != nil {
		t.Fatal(err)
	}
}
