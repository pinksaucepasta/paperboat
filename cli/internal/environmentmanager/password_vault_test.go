package environmentmanager

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/environmente2ee"
)

type secureMemoryStore struct {
	values map[string]string
	getErr error
}

func (*secureMemoryStore) EnvironmentSecureStore() {}
func (store *secureMemoryStore) Set(ref, value string) error {
	if store.values == nil {
		store.values = make(map[string]string)
	}
	store.values[ref] = value
	return nil
}
func (store *secureMemoryStore) Get(ref string) (string, error) {
	value, ok := store.values[ref]
	if !ok {
		if store.getErr != nil {
			return "", store.getErr
		}
		return "", config.ErrSecretNotFound
	}
	return value, nil
}
func (store *secureMemoryStore) Delete(ref string) error { delete(store.values, ref); return nil }

func TestPasswordVaultInitializationDoesNotTreatMixedStoreFailureAsMissing(t *testing.T) {
	secrets := &secureMemoryStore{getErr: errors.Join(config.ErrSecretNotFound, syscall.EIO)}
	store := config.ProfileStore{Path: filepath.Join(t.TempDir(), "profiles.json"), Secrets: secrets}
	control := &vaultControl{store: store}
	vault := PasswordVault{WorkspaceID: "personal", Client: control, Store: store, Issuer: "https://control.example", AccountID: "account_1"}

	err := vault.Initialize(context.Background(), []byte("test master password"))
	if !errors.Is(err, config.ErrSecretNotFound) || !errors.Is(err, syscall.EIO) {
		t.Fatalf("initialization did not preserve the mixed secure-store failure: %v", err)
	}
	if len(secrets.values) != 0 || control.state.DocumentID != "" {
		t.Fatal("mixed secure-store failure authorized vault initialization")
	}
}

type vaultTestCauseTree struct{ causes []error }

func (vaultTestCauseTree) Error() string        { return "test cause tree" }
func (tree vaultTestCauseTree) Unwrap() []error { return tree.causes }

type vaultTestCauseChain struct{ cause error }

func (vaultTestCauseChain) Error() string       { return "test cause chain" }
func (chain vaultTestCauseChain) Unwrap() error { return chain.cause }

type vaultTestCauseCycle struct{ cause error }

func (vaultTestCauseCycle) Error() string        { return "test cause cycle" }
func (cycle *vaultTestCauseCycle) Unwrap() error { return cycle.cause }

func TestVaultExpectedAbsenceProofRejectsMixedAndUnboundedCauses(t *testing.T) {
	if !vaultCredentialAbsentOnly(vaultTestCauseTree{causes: []error{config.ErrSecretNotFound, config.ErrSecretNotFound}}) {
		t.Fatal("uncomparable wrapper with only missing-secret causes was rejected")
	}
	if !vaultCredentialAbsentOnly(&os.PathError{Op: "open", Err: syscall.ENOENT}) {
		t.Fatal("wrapped filesystem absence was rejected")
	}
	if vaultCredentialAbsentOnly(errors.Join(config.ErrSecretNotFound, syscall.EIO)) {
		t.Fatal("mixed missing-secret and I/O failure authorized absence")
	}
	if !vaultAPIResourceAbsentOnly(vaultTestCauseChain{cause: &api.APIError{Status: 404}}) {
		t.Fatal("pure wrapped not-found status was rejected")
	}
	if vaultAPIResourceAbsentOnly(errors.Join(&api.APIError{Status: 404}, syscall.EIO)) {
		t.Fatal("mixed not-found status and I/O failure authorized absence")
	}
	var typedNil *vaultTestCauseCycle
	if vaultCredentialAbsentOnly(typedNil) {
		t.Fatal("typed-nil cause authorized absence")
	}
	cycle := &vaultTestCauseCycle{}
	cycle.cause = cycle
	if vaultCredentialAbsentOnly(cycle) {
		t.Fatal("cyclic cause authorized absence")
	}
	var oversized error = config.ErrSecretNotFound
	for i := 0; i < 16; i++ {
		oversized = vaultTestCauseChain{cause: oversized}
	}
	if vaultCredentialAbsentOnly(oversized) {
		t.Fatal("oversized cause tree authorized absence")
	}
}

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

func TestPasswordVaultOfflineMachineAndInterruptedRewrap(t *testing.T) {
	ctx := context.Background()
	store := config.ProfileStore{Path: filepath.Join(t.TempDir(), "profiles.json"), Secrets: &secureMemoryStore{}}
	control := &vaultControl{store: store}
	a := PasswordVault{WorkspaceID: "personal", Client: control, Store: store, Issuer: "https://control.example", AccountID: "account_1"}
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
		t.Fatal("offline-machine unlock changed key material")
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
	if err := store.LockPasswordVault(a.Issuer, a.AccountID); err != nil {
		t.Fatal(err)
	}
	locked, err := store.LoadPasswordVault(a.Issuer, a.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Clear()
	if len(locked.Payload) != 0 || len(locked.Pending.Payload) != 0 {
		t.Fatal("lock retained interrupted rewrap keys")
	}
	if err := a.Unlock(ctx, []byte("wrong")); !errors.Is(err, environmente2ee.ErrVaultUnlock) {
		t.Fatalf("wrong password pending unlock: %v", err)
	}
	unchanged, err := store.LoadPasswordVault(a.Issuer, a.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	defer unchanged.Clear()
	if unchanged.Head != locked.Head || unchanged.Pending.Head != locked.Pending.Head || !bytes.Equal(unchanged.Envelope, locked.Envelope) || !bytes.Equal(unchanged.Pending.Envelope, locked.Pending.Envelope) || len(unchanged.Payload) != 0 || len(unchanged.Pending.Payload) != 0 {
		t.Fatal("wrong password changed interrupted custody")
	}
	// Another authenticated successor must not replace this pending journal.
	protection, err := environmente2ee.PasswordVaultProtection(locked.Pending.Envelope)
	if err != nil {
		t.Fatal(err)
	}
	forkRaw, forkHead, err := environmente2ee.SealProtectedVault(ctx, environmente2ee.VaultHead{Issuer: a.Issuer, AccountID: a.AccountID, Generation: locked.Pending.Head.Generation + 1}, locked.Pending.Head.ID, protection, initial.Payload)
	if err != nil {
		t.Fatal(err)
	}
	authoritative := control.state
	control.state = api.PasswordVaultState{Issuer: forkHead.Issuer, AccountID: forkHead.AccountID, Generation: forkHead.Generation, DocumentID: forkHead.ID.String(), Envelope: base64.RawURLEncoding.EncodeToString(forkRaw)}
	forkErr := a.Unlock(ctx, newPassword)
	control.state = authoritative
	if !errors.Is(forkErr, ErrVaultChanged) {
		t.Fatalf("unrelated authority accepted for pending unlock: %v", forkErr)
	}
	forkRejected, err := store.LoadPasswordVault(a.Issuer, a.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	defer forkRejected.Clear()
	if forkRejected.Head != locked.Head || forkRejected.Pending.Head != locked.Pending.Head || !bytes.Equal(forkRejected.Pending.Envelope, locked.Pending.Envelope) || len(forkRejected.Payload) != 0 || len(forkRejected.Pending.Payload) != 0 {
		t.Fatal("unrelated authority changed interrupted custody")
	}
	if err := a.Unlock(ctx, newPassword); err != nil {
		t.Fatalf("unlock locked interrupted rewrap: %v", err)
	}
	restored, err := store.LoadPasswordVault(a.Issuer, a.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Clear()
	if restored.Pending.Head != locked.Pending.Head || !bytes.Equal(restored.Pending.Envelope, locked.Pending.Envelope) || !bytes.Equal(restored.Pending.Payload, initial.Payload) {
		t.Fatal("unlock replaced successor or lost keys")
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
	a := PasswordVault{WorkspaceID: "personal", Client: control, Store: store, Issuer: "https://control.example", AccountID: "account_1"}
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
	if err := b.Store.LockPasswordVault(b.Issuer, b.AccountID); err != nil {
		t.Fatal(err)
	}
	if err := b.Unlock(ctx, []byte("new master")); err != nil {
		t.Fatalf("unlock locked interrupted recovery: %v", err)
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

// Lose the acknowledgement after a real staged scope publication, not before
// the manager has a resumable encrypted operation.
type interruptedUnlockScopeControl struct {
	*vaultScopesControl
	interrupt bool
}

func (c *interruptedUnlockScopeControl) PutVaultScope(ctx context.Context, kind, owner, machine string, in api.VaultScopePut) (api.VaultScopeState, error) {
	out, err := c.vaultScopesControl.PutVaultScope(ctx, kind, owner, machine, in)
	if err == nil && c.interrupt {
		c.interrupt = false
		return api.VaultScopeState{}, errors.New("scope acknowledgement lost")
	}
	return out, err
}

func TestPasswordVaultUnlockPreservesInterruptedScopePublication(t *testing.T) {
	ctx := context.Background()
	v, base, store := newVaultScopesFixture(t)
	control := &interruptedUnlockScopeControl{vaultScopesControl: base, interrupt: true}
	v.Client = control
	if err := v.MutateScope(ctx, "personal", v.AccountID, "", func(values map[string][]byte) error {
		values["KEPT"] = []byte("scope-retry-value")
		return nil
	}); err == nil {
		t.Fatal("lost scope acknowledgement not reported")
	}
	pending, err := store.LoadPasswordVault(v.Issuer, v.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	defer pending.Clear()
	if pending.Pending != nil || pending.Operation == nil {
		t.Fatal("scope publication did not retain a source-only operation")
	}
	request := bytes.Clone(pending.Operation.Request)
	if err := store.LockPasswordVault(v.Issuer, v.AccountID); err != nil {
		t.Fatal(err)
	}
	if err := v.Unlock(ctx, []byte("test master password")); err != nil {
		t.Fatalf("unlock interrupted source operation: %v", err)
	}
	restored, err := store.LoadPasswordVault(v.Issuer, v.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Clear()
	if restored.Operation == nil || !bytes.Equal(restored.Operation.Request, request) || !bytes.Equal(restored.Payload, pending.Payload) {
		t.Fatal("unlock discarded encrypted operation or changed account keys")
	}
	if err := v.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	// The interrupted write staged the scope anchor before its record edit.
	// Resume completes that exact anchor; subsequent management uses the
	// restored keys to publish and read an actual variable.
	if err := v.MutateScope(ctx, "personal", v.AccountID, "", func(values map[string][]byte) error {
		values["AFTER"] = []byte("post-resume-value")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	names, err := v.ListScopeNames(ctx, "personal", v.AccountID, "")
	if err != nil || len(names) != 1 || names[0] != "AFTER" {
		t.Fatalf("resumed scope management is not usable: %v", err)
	}
}
