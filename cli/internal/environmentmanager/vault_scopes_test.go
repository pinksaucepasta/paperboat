package environmentmanager

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/environmente2ee"
)

const (
	vaultScopesTestIssuer  = "https://control.example"
	vaultScopesTestAccount = "account_1"
)

type vaultScopesSecretStore struct {
	values map[string]string
}

func (*vaultScopesSecretStore) EnvironmentSecureStore() {}

func (s *vaultScopesSecretStore) Set(ref, value string) error {
	if s.values == nil {
		s.values = make(map[string]string)
	}
	s.values[ref] = value
	return nil
}

func (s *vaultScopesSecretStore) Get(ref string) (string, error) {
	value, ok := s.values[ref]
	if !ok {
		return "", config.ErrSecretNotFound
	}
	return value, nil
}

func (s *vaultScopesSecretStore) Delete(ref string) error {
	delete(s.values, ref)
	return nil
}

// vaultScopesControl is deliberately strict. Methods outside the scenario
// under test fail instead of silently making a mock-only path look complete.
type vaultScopesControl struct {
	store   config.ProfileStore
	issuer  string
	account string

	state  api.PasswordVaultState
	scopes map[string]api.VaultScopeState
	teams  map[string]api.VaultTeamState
	grants []api.VaultGrantState

	writerPublic []byte

	createRequests []api.VaultTeamCreate
	scopePuts      []api.VaultScopePut
	putVaultCount  int
	ackCount       int
	events         []string

	failCreateAfterCommit bool
	failAckAfterCommit    bool
	ackCommitted          bool
	sawAtomicCreate       bool
	sawScopeOperation     bool
	canary                []byte
}

func scopeControlKey(kind, owner, machine string) string {
	return kind + "\x00" + owner + "\x00" + machine
}

func passwordVaultState(raw []byte) (api.PasswordVaultState, error) {
	head, _, err := environmente2ee.InspectPasswordVault(raw)
	if err != nil {
		return api.PasswordVaultState{}, err
	}
	return api.PasswordVaultState{
		Issuer:     head.Issuer,
		AccountID:  head.AccountID,
		Generation: head.Generation,
		DocumentID: head.ID.String(),
		Envelope:   base64.RawURLEncoding.EncodeToString(raw),
	}, nil
}

func decodeVaultTestEnvelope(encoded string) ([]byte, error) {
	return base64.RawURLEncoding.Strict().DecodeString(encoded)
}

func (c *vaultScopesControl) GetPasswordVault(context.Context) (api.PasswordVaultState, error) {
	if c.state.DocumentID == "" {
		return api.PasswordVaultState{}, &api.APIError{Status: 404}
	}
	return c.state, nil
}

func (c *vaultScopesControl) PutPasswordVault(_ context.Context, raw []byte) (api.PasswordVaultState, error) {
	local, err := c.store.LoadPasswordVault(c.issuer, c.account)
	if err != nil {
		return api.PasswordVaultState{}, err
	}
	defer local.Clear()
	if local.Pending == nil || !bytes.Equal(raw, local.Pending.Envelope) {
		return api.PasswordVaultState{}, errors.New("password vault was not staged before publication")
	}
	state, err := passwordVaultState(raw)
	if err != nil {
		return api.PasswordVaultState{}, err
	}
	c.state = state
	c.putVaultCount++
	c.events = append(c.events, "vault")
	return state, nil
}

func (c *vaultScopesControl) GetVaultScope(_ context.Context, kind, owner, machine string) (api.VaultScopeState, error) {
	state, ok := c.scopes[scopeControlKey(kind, owner, machine)]
	if !ok {
		return api.VaultScopeState{}, &api.APIError{Status: 404}
	}
	return state, nil
}

func (c *vaultScopesControl) scopeState(kind, owner, machine, encoded string) (api.VaultScopeState, error) {
	raw, err := decodeVaultTestEnvelope(encoded)
	if err != nil {
		return api.VaultScopeState{}, err
	}
	scope, err := environmente2ee.ParseVaultScope(raw, c.writerPublic)
	if err != nil {
		return api.VaultScopeState{}, err
	}
	claims := scope.Claims
	return api.VaultScopeState{
		OwnerKind:    kind,
		OwnerID:      owner,
		MachineID:    machine,
		KeyEpoch:     claims.KeyEpoch,
		Revision:     claims.Revision,
		DocumentID:   scope.ID.String(),
		Envelope:     encoded,
		WriterPublic: base64.RawURLEncoding.EncodeToString(c.writerPublic),
	}, nil
}

func (c *vaultScopesControl) PutVaultScope(_ context.Context, kind, owner, machine string, in api.VaultScopePut) (api.VaultScopeState, error) {
	local, err := c.store.LoadPasswordVault(c.issuer, c.account)
	if err != nil {
		return api.VaultScopeState{}, err
	}
	if local.Operation == nil || local.Operation.Kind != "scope-put" || local.Pending != nil || len(c.canary) > 0 && bytes.Contains(local.Operation.Request, c.canary) {
		local.Clear()
		return api.VaultScopeState{}, errors.New("scope operation was not encrypted and staged")
	}
	c.sawScopeOperation = true
	local.Clear()

	raw, err := decodeVaultTestEnvelope(in.Envelope)
	if err != nil {
		return api.VaultScopeState{}, err
	}
	if len(c.canary) > 0 && bytes.Contains(raw, c.canary) {
		return api.VaultScopeState{}, errors.New("scope ciphertext contains plaintext canary")
	}
	state, err := c.scopeState(kind, owner, machine, in.Envelope)
	if err != nil {
		return api.VaultScopeState{}, err
	}
	c.scopePuts = append(c.scopePuts, in)
	c.scopes[scopeControlKey(kind, owner, machine)] = state
	c.events = append(c.events, "scope")
	return state, nil
}

func (c *vaultScopesControl) GetVaultSharing(context.Context, string) (api.VaultSharingState, error) {
	return api.VaultSharingState{}, errors.New("unexpected GetVaultSharing")
}

func (c *vaultScopesControl) GetVaultTeam(_ context.Context, teamID string) (api.VaultTeamState, error) {
	team, ok := c.teams[teamID]
	if !ok {
		return api.VaultTeamState{}, &api.APIError{Status: 404}
	}
	return team, nil
}

func (c *vaultScopesControl) CreateVaultTeam(_ context.Context, in api.VaultTeamCreate) (api.VaultTeamState, error) {
	local, err := c.store.LoadPasswordVault(c.issuer, c.account)
	if err != nil {
		return api.VaultTeamState{}, err
	}
	defer local.Clear()
	if local.Pending == nil || local.Operation == nil || local.Operation.Kind != "team-create" {
		return api.VaultTeamState{}, errors.New("team create was not durably staged")
	}
	pendingRaw, err := decodeVaultTestEnvelope(in.VaultEnvelope)
	if err != nil || !bytes.Equal(pendingRaw, local.Pending.Envelope) {
		return api.VaultTeamState{}, errors.New("team create did not submit the staged vault")
	}
	c.sawAtomicCreate = true
	c.createRequests = append(c.createRequests, in)
	c.events = append(c.events, "create")

	state, err := passwordVaultState(pendingRaw)
	if err != nil {
		return api.VaultTeamState{}, err
	}
	c.state = state
	scope, err := c.scopeState("team", in.TeamID, "", in.ScopeEnvelope)
	if err != nil {
		return api.VaultTeamState{}, err
	}
	c.scopes[scopeControlKey("team", in.TeamID, "")] = scope
	c.teams[in.TeamID] = api.VaultTeamState{
		TeamID:       in.TeamID,
		OwnerAccount: c.account,
		Generation:   1,
		KeyEpoch:     scope.KeyEpoch,
		Members: []api.VaultTeamMember{{
			EnvPermission:        "write",
			AccountID:            c.account,
			MembershipGeneration: 1,
			Role:                 "owner",
			Active:               true,
			GrantEpoch:           scope.KeyEpoch,
		}},
		Scope: scope,
	}
	if c.failCreateAfterCommit {
		c.failCreateAfterCommit = false
		return api.VaultTeamState{}, errors.New("connection lost after team create commit")
	}
	return c.teams[in.TeamID], nil
}

func (c *vaultScopesControl) GrantVaultTeamMember(context.Context, string, api.VaultMemberGrant) (api.VaultTeamState, error) {
	return api.VaultTeamState{}, errors.New("unexpected GrantVaultTeamMember")
}

func (c *vaultScopesControl) RotateVaultTeam(context.Context, string, api.VaultTeamRotate) (api.VaultTeamState, error) {
	return api.VaultTeamState{}, errors.New("unexpected RotateVaultTeam")
}

func (c *vaultScopesControl) GetVaultGrants(context.Context) ([]api.VaultGrantState, error) {
	if c.ackCommitted {
		return nil, nil
	}
	return append([]api.VaultGrantState(nil), c.grants...), nil
}

func (c *vaultScopesControl) AckVaultGrant(_ context.Context, digest, vaultID string) error {
	if c.state.DocumentID != vaultID {
		return errors.New("grant acknowledged before vault commit")
	}
	c.ackCount++
	c.events = append(c.events, "ack")
	if digest == "" {
		return errors.New("empty grant digest")
	}
	if c.failAckAfterCommit {
		c.failAckAfterCommit = false
		c.ackCommitted = true
		return errors.New("connection lost after grant acknowledgement")
	}
	c.ackCommitted = true
	return nil
}

func newVaultScopesFixture(t *testing.T) (PasswordVault, *vaultScopesControl, config.ProfileStore) {
	t.Helper()
	store := config.ProfileStore{
		Path:    filepath.Join(t.TempDir(), "profiles.json"),
		Secrets: &vaultScopesSecretStore{},
	}
	control := &vaultScopesControl{
		store:   store,
		issuer:  vaultScopesTestIssuer,
		account: vaultScopesTestAccount,
		scopes:  make(map[string]api.VaultScopeState),
		teams:   make(map[string]api.VaultTeamState),
	}
	vault := PasswordVault{Client: control, Store: store, Issuer: control.issuer, AccountID: control.account}
	if err := vault.InitializeWithRecovery(context.Background(), []byte("test master password"), nil); err != nil {
		t.Fatal(err)
	}
	record, err := store.LoadPasswordVault(control.issuer, control.account)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := environmente2ee.ParseVaultKeys(record.Payload)
	record.Clear()
	if err != nil {
		t.Fatal(err)
	}
	signer := ed25519.NewKeyFromSeed(keys.WriterSeed)
	control.writerPublic = append([]byte(nil), signer.Public().(ed25519.PublicKey)...)
	clear(signer)
	keys.Clear()
	control.events = nil
	control.putVaultCount = 0
	return vault, control, store
}

func loadVaultKeysForScopeTest(t *testing.T, store config.ProfileStore) (config.PasswordVaultRecord, environmente2ee.VaultKeys) {
	t.Helper()
	record, err := store.LoadPasswordVault(vaultScopesTestIssuer, vaultScopesTestAccount)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := environmente2ee.ParseVaultKeys(record.Payload)
	if err != nil {
		record.Clear()
		t.Fatal(err)
	}
	return record, keys
}

func TestVaultScopesCreateStagesAtomicOperationAndResumesExactRequest(t *testing.T) {
	vault, control, store := newVaultScopesFixture(t)
	initial, keys := loadVaultKeysForScopeTest(t, store)
	initialHead := initial.Head
	initial.Clear()
	keys.Clear()

	control.failCreateAfterCommit = true
	err := vault.CreateTeamAt(context.Background(), "team_alpha", 9, 4)
	if err == nil {
		t.Fatal("lost team-create response was hidden")
	}
	if !control.sawAtomicCreate || len(control.createRequests) != 1 || len(control.events) != 1 || control.events[0] != "create" {
		t.Fatalf("create staging/network sequence: atomic=%v requests=%d events=%v", control.sawAtomicCreate, len(control.createRequests), control.events)
	}
	local, err := store.LoadPasswordVault(vaultScopesTestIssuer, vaultScopesTestAccount)
	if err != nil {
		t.Fatal(err)
	}
	if local.Pending == nil || local.Operation == nil || local.Operation.Kind != "team-create" {
		local.Clear()
		t.Fatal("team create did not retain both pending vault and opaque operation")
	}
	first := control.createRequests[0]
	if first.ExpectedTeamGeneration != 9 {
		t.Fatalf("expected team generation = %d, want 9", first.ExpectedTeamGeneration)
	}
	requestBytes := append([]byte(nil), local.Operation.Request...)
	local.Clear()
	if bytes.Contains(requestBytes, []byte("test master password")) {
		clear(requestBytes)
		t.Fatal("team operation retained a password")
	}
	clear(requestBytes)

	if err := vault.Resume(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(control.createRequests) != 2 || control.createRequests[1] != first {
		t.Fatalf("resume changed the staged team-create request: %#v", control.createRequests)
	}
	if len(control.events) != 2 || control.events[1] != "create" || control.putVaultCount != 0 {
		t.Fatalf("resume used an extra or wrong publication path: events=%v puts=%d", control.events, control.putVaultCount)
	}

	final, err := store.LoadPasswordVault(vaultScopesTestIssuer, vaultScopesTestAccount)
	if err != nil {
		t.Fatal(err)
	}
	defer final.Clear()
	if final.Pending != nil || final.Operation != nil || final.Head.Generation != initialHead.Generation+1 || final.Head != controlStateHead(control) {
		t.Fatalf("team resume did not commit the exact successor: head=%+v remote=%+v pending=%v operation=%v", final.Head, controlStateHead(control), final.Pending, final.Operation)
	}
	finalKeys, err := environmente2ee.ParseVaultKeys(final.Payload)
	if err != nil {
		t.Fatal(err)
	}
	defer finalKeys.Clear()
	if len(finalKeys.Teams) != 1 || finalKeys.Teams[0].TeamID != "team_alpha" || finalKeys.Teams[0].Epoch != 1 || finalKeys.Teams[0].MembershipGeneration != 4 {
		t.Fatalf("resumed team key inventory = %+v", finalKeys.Teams)
	}
}

func controlStateHead(control *vaultScopesControl) environmente2ee.VaultHead {
	head, _, err := control.state.Decode()
	if err != nil {
		return environmente2ee.VaultHead{}
	}
	return head
}

func TestVaultTeamActorSeparatesReadFromWritePermission(t *testing.T) {
	team := api.VaultTeamState{Members: []api.VaultTeamMember{{AccountID: "admin_1", Role: "admin", Active: true, EnvPermission: "read"}}}
	if actor, ok := vaultTeamActor(team, "admin_1", "read"); !ok || actor.Role != "admin" {
		t.Fatal("active admin with read permission was denied key operation")
	}
	if _, ok := vaultTeamActor(team, "admin_1", "write"); ok {
		t.Fatal("read permission authorized ordinary scope write")
	}
	team.Members[0].EnvPermission = ""
	if _, ok := vaultTeamActor(team, "admin_1", "read"); ok {
		t.Fatal("role without explicit ENV permission authorized key operation")
	}
}

func TestVaultTeamRemovalAcceptsKnownInactiveRevokedMember(t *testing.T) {
	members := []api.VaultTeamMember{
		{AccountID: "owner_1", Role: "owner", Active: true, EnvPermission: "write"},
		{AccountID: "member_1", Role: "member", Active: false},
		{AccountID: "admin_1", Role: "admin", Active: false},
	}
	if !validVaultTeamRemovals(members, "owner", map[string]bool{"member_1": true}) {
		t.Fatal("owner could not complete rekey for a known inactive revoked member")
	}
	if validVaultTeamRemovals(members, "admin", map[string]bool{"admin_1": true}) {
		t.Fatal("admin could complete removal rekey for another admin")
	}
	if validVaultTeamRemovals(members, "owner", map[string]bool{"missing": true}) {
		t.Fatal("unknown removal target was accepted")
	}
}

func TestVaultScopesMutationPublishesCiphertextWithoutPlaintext(t *testing.T) {
	vault, control, store := newVaultScopesFixture(t)
	record, keys := loadVaultKeysForScopeTest(t, store)
	personalKey := bytes.Clone(keys.PersonalKey)
	record.Clear()
	keys.Clear()
	defer clear(personalKey)

	canary := []byte("scope plaintext must never cross the client boundary")
	control.canary = canary
	var callbackValue []byte
	err := vault.MutateScope(context.Background(), "personal", vaultScopesTestAccount, "", func(values map[string][]byte) error {
		values["APP_SECRET"] = append([]byte(nil), canary...)
		callbackValue = values["APP_SECRET"]
		return nil
	})
	if err != nil {
		clear(callbackValue)
		t.Fatal(err)
	}
	defer clear(callbackValue)
	if !allZeroBytes(callbackValue) {
		t.Fatal("scope mutation retained callback plaintext after sealing")
	}
	if !control.sawScopeOperation || len(control.scopePuts) != 1 || len(control.events) != 1 || control.events[0] != "scope" {
		t.Fatalf("scope staging/publication sequence: staged=%v puts=%d events=%v", control.sawScopeOperation, len(control.scopePuts), control.events)
	}
	requestJSON, err := json.Marshal(control.scopePuts[0])
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(requestJSON, canary) {
		t.Fatal("scope API request contained plaintext canary")
	}
	raw, err := decodeVaultTestEnvelope(control.scopePuts[0].Envelope)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, canary) {
		t.Fatal("scope ciphertext contained plaintext canary")
	}

	state := control.scopes[scopeControlKey("personal", vaultScopesTestAccount, "")]
	scope, err := state.Decode()
	if err != nil {
		t.Fatal(err)
	}
	values, err := environmente2ee.OpenVaultScope(context.Background(), scope, personalKey)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(values["APP_SECRET"], canary) {
		clearScopeTestValues(values)
		t.Fatalf("native scope decode returned %q", values["APP_SECRET"])
	}
	clearScopeTestValues(values)

	final, err := store.LoadPasswordVault(vaultScopesTestIssuer, vaultScopesTestAccount)
	if err != nil {
		t.Fatal(err)
	}
	defer final.Clear()
	if final.Pending != nil || final.Operation != nil {
		t.Fatal("successful scope mutation left local publication state")
	}
}

func allZeroBytes(value []byte) bool {
	for _, b := range value {
		if b != 0 {
			return false
		}
	}
	return true
}

func clearScopeTestValues(values map[string][]byte) {
	for _, value := range values {
		clear(value)
	}
}

func configureIncomingGrant(t *testing.T, control *vaultScopesControl, store config.ProfileStore, active bool, memberGeneration uint64) []byte {
	t.Helper()
	record, keys := loadVaultKeysForScopeTest(t, store)
	defer record.Clear()
	defer keys.Clear()
	sharing, err := ecdh.X25519().NewPrivateKey(keys.SharingPrivate)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(sharing.Bytes())
	teamKey := bytes.Repeat([]byte{0x5c}, 32)
	grant, err := environmente2ee.SealTeamGrant(context.Background(), environmente2ee.TeamGrantClaims{
		Issuer:                   vaultScopesTestIssuer,
		TeamID:                   "team_sync",
		TeamEpoch:                2,
		MembershipGeneration:     1,
		SenderAccount:            "account_owner",
		RecipientAccount:         vaultScopesTestAccount,
		RecipientVaultGeneration: record.Head.Generation,
		RecipientSharingPublic:   sharing.PublicKey().Bytes(),
		OperationID:              "envop_sync_test",
	}, teamKey, keys.WriterSeed)
	if err != nil {
		t.Fatal(err)
	}
	writer := ed25519.NewKeyFromSeed(keys.WriterSeed)
	writerPublic := append([]byte(nil), writer.Public().(ed25519.PublicKey)...)
	clear(writer)
	control.grants = []api.VaultGrantState{{
		TeamID:               "team_sync",
		MembershipGeneration: 1,
		TeamEpoch:            2,
		DocumentID:           grant.ID.String(),
		Envelope:             base64.RawURLEncoding.EncodeToString(grant.Raw),
		SenderWriterPublic:   base64.RawURLEncoding.EncodeToString(writerPublic),
	}}
	control.teams["team_sync"] = api.VaultTeamState{
		TeamID:       "team_sync",
		OwnerAccount: "account_owner",
		Generation:   4,
		KeyEpoch:     2,
		Members: []api.VaultTeamMember{{
			EnvPermission:        "write",
			AccountID:            vaultScopesTestAccount,
			MembershipGeneration: memberGeneration,
			Role:                 "member",
			Active:               active,
			GrantEpoch:           2,
		}},
	}
	return teamKey
}

func TestSyncTeamGrantsCommitsVaultBeforeAckAndDoesNotRepeatAfterLostAck(t *testing.T) {
	vault, control, store := newVaultScopesFixture(t)
	initial, err := store.LoadPasswordVault(vaultScopesTestIssuer, vaultScopesTestAccount)
	if err != nil {
		t.Fatal(err)
	}
	initialHead := initial.Head
	initial.Clear()
	teamKey := configureIncomingGrant(t, control, store, true, 1)
	defer clear(teamKey)
	control.failAckAfterCommit = true

	err = vault.SyncTeamGrants(context.Background())
	if err == nil {
		t.Fatal("lost grant acknowledgement was hidden")
	}
	if control.putVaultCount != 1 || control.ackCount != 1 || len(control.events) != 2 || control.events[0] != "vault" || control.events[1] != "ack" {
		t.Fatalf("grant commit/ack sequence = puts=%d acks=%d events=%v", control.putVaultCount, control.ackCount, control.events)
	}
	committed, keys := loadVaultKeysForScopeTest(t, store)
	if committed.Pending != nil || committed.Operation != nil || committed.Head.Generation != initialHead.Generation+1 {
		committed.Clear()
		keys.Clear()
		t.Fatalf("grant failure did not leave committed successor: head=%+v pending=%v operation=%v", committed.Head, committed.Pending, committed.Operation)
	}
	if len(keys.Teams) != 1 || keys.Teams[0].TeamID != "team_sync" || keys.Teams[0].Epoch != 2 || !bytes.Equal(keys.Teams[0].Key, teamKey) {
		committed.Clear()
		keys.Clear()
		t.Fatalf("grant key was not committed before acknowledgement: %+v", keys.Teams)
	}
	committed.Clear()
	keys.Clear()

	if err := vault.SyncTeamGrants(context.Background()); err != nil {
		t.Fatal(err)
	}
	if control.putVaultCount != 1 || control.ackCount != 1 || len(control.events) != 2 {
		t.Fatalf("retry repeated key publication after lost ack: puts=%d acks=%d events=%v", control.putVaultCount, control.ackCount, control.events)
	}
}

func TestSyncTeamGrantsRejectsRevokedOrStaleMembership(t *testing.T) {
	tests := []struct {
		name             string
		active           bool
		memberGeneration uint64
	}{
		{name: "revoked", active: false, memberGeneration: 1},
		{name: "stale_membership", active: true, memberGeneration: 2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			vault, control, store := newVaultScopesFixture(t)
			initial, err := store.LoadPasswordVault(vaultScopesTestIssuer, vaultScopesTestAccount)
			if err != nil {
				t.Fatal(err)
			}
			initialHead := initial.Head
			initialPayload := append([]byte(nil), initial.Payload...)
			initial.Clear()
			teamKey := configureIncomingGrant(t, control, store, test.active, test.memberGeneration)
			clear(teamKey)

			err = vault.SyncTeamGrants(context.Background())
			if err == nil {
				t.Fatal("revoked or stale membership was accepted")
			}
			if control.putVaultCount != 0 || control.ackCount != 0 || len(control.events) != 0 {
				t.Fatalf("stale grant caused mutation: puts=%d acks=%d events=%v", control.putVaultCount, control.ackCount, control.events)
			}
			after, err := store.LoadPasswordVault(vaultScopesTestIssuer, vaultScopesTestAccount)
			if err != nil {
				clear(initialPayload)
				t.Fatal(err)
			}
			defer after.Clear()
			defer clear(initialPayload)
			if after.Head != initialHead || after.Pending != nil || after.Operation != nil || !bytes.Equal(after.Payload, initialPayload) {
				t.Fatalf("stale grant changed local custody: head=%+v pending=%v operation=%v", after.Head, after.Pending, after.Operation)
			}
		})
	}
}

var _ PasswordVaultClient = (*vaultScopesControl)(nil)
var _ VaultDataClient = (*vaultScopesControl)(nil)
