package main

import (
	"bytes"
	"context"
	"crypto/ecdh"
	env "github.com/pinksaucepasta/paperboat/internal/environmente2ee"
	"strings"
	"testing"
)

func stateFrom(t *testing.T, raw []byte) vaultState {
	t.Helper()
	h, _, err := env.InspectPasswordVault(raw)
	if err != nil {
		t.Fatal(err)
	}
	return vaultState{Issuer: h.Issuer, Account: h.AccountID, Generation: h.Generation, DocumentID: digest(h.ID), Envelope: encoded(raw)}
}
func TestBrowserVaultNativeInteroperabilityAndFencing(t *testing.T) {
	ctx := context.Background()
	e := &engine{}
	defer e.clear()
	_, err := e.run(ctx, request{Action: "initialize", Vault: vaultState{Issuer: "https://control.example", Account: "account-one"}, Password: "test browser password"})
	if err != nil {
		t.Fatal(err)
	}
	initial := stateFrom(t, e.pendingRaw)
	if _, err := e.run(ctx, request{Action: "commit", Vault: initial}); err != nil {
		t.Fatal(err)
	}
	result, err := e.run(ctx, request{Action: "set", Vault: initial, WorkspaceID: "personal", Kind: "personal", Owner: initial.Account, Name: "TOKEN", Value: "private-value"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := decode(result.(map[string]string)["envelope"])
	protection, _ := env.PasswordVaultProtection(e.raw)
	scope, err := env.ParseVaultScope(raw, protection.WriterPublic)
	if err != nil {
		t.Fatal(err)
	}
	values, err := env.OpenVaultScope(ctx, scope, e.keys.PersonalKey)
	if err != nil || string(values["TOKEN"]) != "private-value" {
		t.Fatalf("native read failed: %v", err)
	}
	for _, v := range values {
		clear(v)
	}
	state := scopeState{WriterPublic: encoded(protection.WriterPublic), WorkspaceID: "personal", Kind: "personal", Owner: initial.Account, Epoch: scope.Claims.KeyEpoch, Revision: scope.Claims.Revision, DocumentID: digest(scope.ID), Envelope: encoded(raw)}
	names, err := e.run(ctx, request{Action: "names", Vault: initial, Scope: &state, WorkspaceID: "personal", Kind: "personal", Owner: initial.Account})
	if err != nil || names.(map[string]any)["names"].([]string)[0] != "TOKEN" {
		t.Fatalf("names failed: %v", err)
	}
	if _, err := e.run(ctx, request{Action: "names", Vault: initial, Scope: &state, WorkspaceID: "personal", Kind: "personal", Owner: "other-account"}); err == nil {
		t.Fatal("cross account accepted")
	}
	if _, err := e.run(ctx, request{Action: "password", Vault: initial, Password: "replacement password"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.run(ctx, request{Action: "set", Vault: initial, WorkspaceID: "personal", Kind: "personal", Owner: initial.Account, Name: "LATE", Value: "secret"}); err == nil {
		t.Fatal("mutation while publication pending accepted")
	}
	next := stateFrom(t, e.pendingRaw)
	if _, err := e.run(ctx, request{Action: "commit", Vault: next}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.OpenPasswordVault(ctx, e.head, []byte("test browser password"), e.raw); err == nil {
		t.Fatal("old password accepted")
	}
	payload, err := env.OpenPasswordVault(ctx, e.head, []byte("replacement password"), e.raw)
	if err != nil {
		t.Fatal(err)
	}
	clear(payload)
	if _, err := e.run(ctx, request{Action: "set", Vault: initial, WorkspaceID: "personal", Kind: "personal", Owner: initial.Account, Name: "STALE", Value: "secret"}); err == nil {
		t.Fatal("stale vault accepted")
	}
	if _, err := e.run(ctx, request{Action: "lock"}); err != nil || len(e.keys.PersonalKey) > 0 || len(e.raw) > 0 {
		t.Fatal("lock retains keys")
	}
}

func initialized(t *testing.T) (*engine, vaultState) {
	t.Helper()
	e := &engine{}
	_, err := e.run(context.Background(), request{Action: "initialize", Vault: vaultState{Issuer: "https://control.example", Account: "account-one"}, Password: "browser password"})
	if err != nil {
		t.Fatal(err)
	}
	v := stateFrom(t, e.pendingRaw)
	if _, err = e.run(context.Background(), request{Action: "commit", Vault: v}); err != nil {
		t.Fatal(err)
	}
	return e, v
}
func TestBrowserTeamMembershipAndNativeScope(t *testing.T) {
	ctx := context.Background()
	e, v := initialized(t)
	defer e.clear()
	team := teamState{ID: "team-one", Owner: v.Account, Generation: 1}
	result, err := e.run(ctx, request{Action: "team-create", Vault: v, Team: &team, Owner: team.ID, Membership: 1, Operation: "operation-team"})
	if err != nil {
		t.Fatal(err)
	}
	payload := result.(map[string]any)
	raw, _ := decode(payload["scope_envelope"].(string))
	protection, _ := env.PasswordVaultProtection(e.raw)
	scope, err := env.ParseVaultScope(raw, protection.WriterPublic)
	if err != nil {
		t.Fatal(err)
	}
	next := stateFrom(t, e.pendingRaw)
	if _, err = e.run(ctx, request{Action: "commit", Vault: next}); err != nil {
		t.Fatal(err)
	}
	// Replaying a verified commit after an acknowledgement failure must be safe.
	if _, err = e.run(ctx, request{Action: "commit", Vault: next}); err != nil {
		t.Fatal(err)
	}
	team.Epoch = 1
	team.Members = []teamMember{{Account: v.Account, Membership: 1, Role: "owner", Active: true, Permission: "write", GrantEpoch: 1}}
	team.Scope = scopeState{WorkspaceID: team.ID, Kind: "team", Owner: team.ID, Epoch: 1, Revision: 1, DocumentID: digest(scope.ID), Envelope: encoded(raw), WriterPublic: encoded(protection.WriterPublic)}
	_, err = e.run(ctx, request{Action: "set", Vault: next, Team: &team, Scope: &team.Scope, WorkspaceID: team.ID, Kind: "team", Owner: team.ID, Name: "TEAM_TOKEN", Value: "private"})
	if err != nil {
		t.Fatal(err)
	}
	team.Members[0].Membership = 2
	if _, err = e.run(ctx, request{Action: "names", Vault: next, Team: &team, Scope: &team.Scope, WorkspaceID: team.ID, Kind: "team", Owner: team.ID}); err == nil {
		t.Fatal("stale membership accepted")
	}
	team.Members[0].Membership = 1
	team.Members[0].Permission = "read"
	if _, err = e.run(ctx, request{Action: "set", Vault: next, Team: &team, Scope: &team.Scope, WorkspaceID: team.ID, Kind: "team", Owner: team.ID, Name: "TOKEN", Value: "secret"}); err == nil {
		t.Fatal("read-only mutation accepted")
	}
}
func TestBrowserResetRejectsInvalidInventoryAndReplacesKeys(t *testing.T) {
	ctx := context.Background()
	e, v := initialized(t)
	defer e.clear()
	oldKey := append([]byte(nil), e.keys.PersonalKey...)
	defer clear(oldKey)
	bad := scopeState{WorkspaceID: "personal", Kind: "personal", Owner: "another-account", Epoch: 1, Revision: 1}
	if _, err := e.run(ctx, request{Action: "reset-vault", Vault: v, Password: "new password", KeyEpoch: 1, Inventory: []scopeState{bad}}); err == nil {
		t.Fatal("foreign reset scope accepted")
	}
	if len(e.pendingRaw) != 0 || len(e.keys.PersonalKey) != 0 {
		t.Fatal("failed reset retained keys")
	}
	if _, err := e.run(ctx, request{Action: "unlock", Vault: v, Password: "browser password"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.run(ctx, request{Action: "reset-vault", Vault: v, Password: "new password", KeyEpoch: 1}); err != nil {
		t.Fatal(err)
	}
	next := stateFrom(t, e.pendingRaw)
	if _, err := e.run(ctx, request{Action: "commit", Vault: next}); err != nil {
		t.Fatal(err)
	}
	if e.keys.PersonalEpoch != 2 || string(oldKey) == string(e.keys.PersonalKey) {
		t.Fatal("reset preserved lost key")
	}
	if _, err := env.OpenPasswordVault(ctx, e.head, []byte("browser password"), e.raw); err == nil {
		t.Fatal("reset retained old password")
	}
	if _, err := e.run(ctx, request{Action: "names", Vault: v, WorkspaceID: "personal", Kind: "personal", Owner: v.Account}); err == nil {
		t.Fatal("stale vault accepted after reset")
	}
	// Switching account must immediately erase unlocked key material.
	other := next
	other.Account = "account-two"
	if _, err := e.run(ctx, request{Action: "names", Vault: other, WorkspaceID: "personal", Kind: "personal", Owner: other.Account}); err == nil {
		t.Fatal("account switch accepted")
	}
	if len(e.keys.PersonalKey) != 0 {
		t.Fatal("account switch retained keys")
	}
}
func TestBrowserPersonalRotationPreservesNativeValues(t *testing.T) {
	ctx := context.Background()
	e, v := initialized(t)
	defer e.clear()
	out, err := e.run(ctx, request{Action: "set", Vault: v, WorkspaceID: "personal", Kind: "personal", Owner: v.Account, Name: "TOKEN", Value: "preserved"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := decode(out.(map[string]string)["envelope"])
	protection, _ := env.PasswordVaultProtection(e.raw)
	doc, err := env.ParseVaultScope(raw, protection.WriterPublic)
	if err != nil {
		t.Fatal(err)
	}
	scope := scopeState{WorkspaceID: "personal", Kind: "personal", Owner: v.Account, Epoch: 1, Revision: 1, DocumentID: digest(doc.ID), WriterPublic: encoded(protection.WriterPublic), Envelope: encoded(raw)}
	if _, err = e.run(ctx, request{Action: "personal-rotate", Vault: v, KeyEpoch: 1, Inventory: []scopeState{scope}}); err != nil {
		t.Fatal(err)
	}
	out, err = e.run(ctx, request{Action: "personal-rotate-scope", Vault: v, Scope: &scope})
	if err != nil {
		t.Fatal(err)
	}
	nextRaw, _ := decode(out.(map[string]any)["envelope"].(string))
	nextProtection, _ := env.PasswordVaultProtection(e.pendingRaw)
	nextDoc, err := env.ParseVaultScope(nextRaw, nextProtection.WriterPublic)
	if err != nil {
		t.Fatal(err)
	}
	values, err := env.OpenVaultScope(ctx, nextDoc, e.pendingKeys.PersonalKey)
	if err != nil || string(values["TOKEN"]) != "preserved" {
		t.Fatal("rotation lost native values")
	}
	for _, value := range values {
		clear(value)
	}
	if nextDoc.Claims.KeyEpoch != 2 || nextDoc.Claims.Revision != 2 || nextDoc.Claims.WriterVaultGeneration != 2 {
		t.Fatal("rotation scope fencing incorrect")
	}
	next := stateFrom(t, e.pendingRaw)
	if _, err = e.run(ctx, request{Action: "commit", Vault: next}); err != nil {
		t.Fatal(err)
	}
	if _, err = env.OpenVaultScope(ctx, doc, e.keys.PersonalKey); err == nil {
		t.Fatal("new key opened old epoch ciphertext")
	}
}
func TestBrowserTeamGrantAcceptAndRotation(t *testing.T) {
	ctx := context.Background()
	alice, a := initialized(t)
	defer alice.clear()
	bob := &engine{}
	defer bob.clear()
	if _, err := bob.run(ctx, request{Action: "initialize", Vault: vaultState{Issuer: a.Issuer, Account: "account-bob"}, Password: "bob password"}); err != nil {
		t.Fatal(err)
	}
	b := stateFrom(t, bob.pendingRaw)
	if _, err := bob.run(ctx, request{Action: "commit", Vault: b}); err != nil {
		t.Fatal(err)
	}
	team := teamState{ID: "team-one", Owner: a.Account, Generation: 1}
	out, err := alice.run(ctx, request{Action: "team-create", Vault: a, Team: &team, Owner: team.ID, Membership: 1, Operation: "operation-create"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := decode(out.(map[string]any)["scope_envelope"].(string))
	writer, _ := env.PasswordVaultProtection(alice.raw)
	scope, err := env.ParseVaultScope(raw, writer.WriterPublic)
	if err != nil {
		t.Fatal(err)
	}
	a = stateFrom(t, alice.pendingRaw)
	if _, err = alice.run(ctx, request{Action: "commit", Vault: a}); err != nil {
		t.Fatal(err)
	}
	team.Epoch = 1
	team.Generation = 2
	team.Members = []teamMember{{Account: a.Account, Membership: 1, Role: "owner", Active: true, Permission: "write", GrantEpoch: 1}, {Account: b.Account, Membership: 1, Role: "member", Active: true, Permission: "read", GrantEpoch: 1}}
	team.Scope = scopeState{WorkspaceID: team.ID, Kind: "team", Owner: team.ID, Epoch: 1, Revision: 1, DocumentID: digest(scope.ID), Envelope: encoded(raw), WriterPublic: encoded(writer.WriterPublic)}
	bobPublic, _ := ecdh.X25519().NewPrivateKey(bob.keys.SharingPrivate)
	sharing := sharingState{Account: b.Account, Generation: b.Generation, Public: encoded(bobPublic.PublicKey().Bytes())}
	out, err = alice.run(ctx, request{Action: "team-grant", Vault: a, Team: &team, Owner: team.ID, Recipient: b.Account, Operation: "operation-grant", Sharing: []sharingState{sharing}})
	if err != nil {
		t.Fatal(err)
	}
	grantRaw, _ := decode(out.(map[string]any)["grant_envelope"].(string))
	grant, err := env.ParseTeamGrant(grantRaw, writer.WriterPublic)
	if err != nil {
		t.Fatal(err)
	}
	item := grantState{Team: team.ID, Membership: 1, Epoch: 1, Document: digest(grant.ID), Envelope: encoded(grantRaw), Writer: encoded(writer.WriterPublic)}
	if _, err = bob.run(ctx, request{Action: "team-accept", Vault: b, Team: &team, Owner: team.ID, Operation: "operation-accept", Grant: &item}); err != nil {
		t.Fatal(err)
	}
	b = stateFrom(t, bob.pendingRaw)
	if _, err = bob.run(ctx, request{Action: "commit", Vault: b}); err != nil {
		t.Fatal(err)
	}
	if _, err = bob.run(ctx, request{Action: "names", Vault: b, WorkspaceID: team.ID, Kind: "team", Owner: team.ID, Team: &team, Scope: &team.Scope}); err != nil {
		t.Fatal("accepted grant cannot open native scope:", err)
	}
	alicePublic, _ := ecdh.X25519().NewPrivateKey(alice.keys.SharingPrivate)
	out, err = alice.run(ctx, request{Action: "team-rotate", Vault: a, Team: &team, Owner: team.ID, Operation: "operation-rotate", Remove: []string{b.Account}, Sharing: []sharingState{{Account: a.Account, Generation: a.Generation, Public: encoded(alicePublic.PublicKey().Bytes())}}})
	if err != nil {
		t.Fatal(err)
	}
	payload := out.(map[string]any)
	if len(payload["grant_envelopes"].([]string)) != 1 {
		t.Fatal("removed member received successor grant")
	}
	rotatedRaw, _ := decode(payload["scope_envelope"].(string))
	rotated, err := env.ParseVaultScope(rotatedRaw, writer.WriterPublic)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Claims.KeyEpoch != 2 {
		t.Fatal("rotation epoch not advanced")
	}
	if _, err = env.OpenVaultScope(ctx, rotated, bob.keys.Teams[0].Key); err == nil {
		t.Fatal("removed member opened successor ciphertext")
	}
}
func TestBrowserReservedNameShowsActionableError(t *testing.T) {
	e, v := initialized(t)
	defer e.clear()
	for _, name := range []string{"PAPERBOAT_API_AUDIT", "paperboat_token", "LD_PRELOAD", "DYLD_LIBRARY_PATH", "NODE_OPTIONS", "PYTHONPATH", "PYTHONHOME", "GOTRACEBACK", "1INVALID"} {
		_, err := e.run(context.Background(), request{Action: "set", Vault: v, WorkspaceID: "personal", Kind: "personal", Owner: v.Account, Name: name, Value: "private"})
		if err == nil || !strings.Contains(err.Error(), "variable name is invalid or reserved") {
			t.Fatalf("%s has no actionable name error", name)
		}
	}
	if _, err := e.run(context.Background(), request{Action: "set", Vault: v, WorkspaceID: "personal", Kind: "personal", Owner: v.Account, Name: "PB_API_AUDIT", Value: "private"}); err != nil {
		t.Fatal(err)
	}
}

func TestBrowserStatusPreservesSameAccountPendingPublication(t *testing.T) {
	ctx := context.Background()
	e, v := initialized(t)
	e.clear() // A total-loss reset is initiated while browser keys are locked.
	defer e.clear()
	if _, err := e.run(ctx, request{Action: "reset-vault", Vault: v, Password: "replacement password", KeyEpoch: 1}); err != nil {
		t.Fatal(err)
	}
	next := stateFrom(t, e.pendingRaw)
	status, err := e.run(ctx, request{Action: "status", Vault: vaultState{Account: v.Account}})
	if err != nil || status.(map[string]any)["unlocked"].(bool) {
		t.Fatal("locked pending reset incorrectly reports unlocked", err)
	}
	if _, err := e.run(ctx, request{Action: "commit", Vault: next}); err != nil {
		t.Fatal("same-account status erased committed reset keys", err)
	}
	if _, err := e.run(ctx, request{Action: "unlock", Vault: next, Password: "replacement password"}); err != nil {
		t.Fatal(err)
	}
	e.clear()
	if _, err := e.run(ctx, request{Action: "initialize", Vault: vaultState{Issuer: v.Issuer, Account: v.Account}, Password: "initial password"}); err != nil {
		t.Fatal(err)
	}
	next = stateFrom(t, e.pendingRaw)
	if _, err := e.run(ctx, request{Action: "status", Vault: vaultState{Account: v.Account}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.run(ctx, request{Action: "commit", Vault: next}); err != nil {
		t.Fatal("same-account status erased initialization keys", err)
	}
	e.clear()
	if _, err := e.run(ctx, request{Action: "initialize", Vault: vaultState{Issuer: v.Issuer, Account: v.Account}, Password: "initial password"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.run(ctx, request{Action: "status", Vault: vaultState{Account: "another-account"}}); err != nil {
		t.Fatal(err)
	}
	if e.pendingKeys != nil || e.pendingRaw != nil || len(e.raw) != 0 {
		t.Fatal("different-account status retained staged keys")
	}
}

func TestBrowserScopeConflictReconciliationVerifiesSignedHeads(t *testing.T) {
	ctx := context.Background()
	e, v := initialized(t)
	defer e.clear()
	prepare := func(scope *scopeState, value string) scopeState {
		t.Helper()
		out, err := e.run(ctx, request{Action: "set", Vault: v, Scope: scope, WorkspaceID: "personal", Kind: "personal", Owner: v.Account, Name: "VALUE", Value: value})
		if err != nil {
			t.Fatal(err)
		}
		metadata, err := e.run(ctx, request{Action: "scope-candidate", Vault: v, Envelope: out.(map[string]string)["envelope"], WorkspaceID: "personal", Kind: "personal", Owner: v.Account})
		if err != nil {
			t.Fatal(err)
		}
		return metadata.(scopeState)
	}
	first := prepare(nil, "first")
	candidate := prepare(&first, "candidate")
	competing := prepare(&first, "competing")
	check := func(current scopeState, expected string) {
		t.Helper()
		out, err := e.run(ctx, request{Action: "scope-reconcile", Vault: v, Candidate: &candidate, Scope: &current, WorkspaceID: "personal", Kind: "personal", Owner: v.Account})
		if expected == "invalid" {
			if err == nil {
				t.Fatal("invalid signed head accepted")
			}
			return
		}
		if err != nil || out.(map[string]string)["state"] != expected {
			t.Fatal("incorrect scope reconciliation", expected, err)
		}
	}
	check(candidate, "committed")
	check(competing, "superseded")
	check(first, "retained")
	newer := prepare(&candidate, "newer")
	check(newer, "superseded")
	parsed, err := scopeDocument(candidate)
	if err != nil {
		t.Fatal(err)
	}
	claims := parsed.Claims
	claims.KeyEpoch++
	claims.Revision = 1
	claims.Previous = make([]byte, 32)
	document, err := env.SealVaultScope(ctx, claims, e.keys.PersonalKey, e.keys.WriterSeed, map[string][]byte{})
	if err != nil {
		t.Fatal(err)
	}
	epoch := candidate
	epoch.Epoch = claims.KeyEpoch
	epoch.Revision = 1
	epoch.DocumentID = digest(document.ID)
	epoch.Envelope = encoded(document.Raw)
	check(epoch, "superseded")
	invalid := competing
	invalid.Owner = "other-account"
	check(invalid, "invalid")
	invalid = competing
	raw, _ := decode(invalid.Envelope)
	raw[len(raw)-1] ^= 1
	invalid.Envelope = encoded(raw)
	check(invalid, "invalid")
	claims.Issuer = "https://other.invalid"
	document, err = env.SealVaultScope(ctx, claims, e.keys.PersonalKey, e.keys.WriterSeed, map[string][]byte{})
	if err != nil {
		t.Fatal(err)
	}
	invalid = epoch
	invalid.DocumentID = digest(document.ID)
	invalid.Envelope = encoded(document.Raw)
	check(invalid, "invalid")
	// Scope reconciliation must never erase a staged vault/key successor.
	if _, err := e.run(ctx, request{Action: "personal-rotate", Vault: v, KeyEpoch: 1}); err != nil {
		t.Fatal(err)
	}
	pendingRaw := append([]byte(nil), e.pendingRaw...)
	defer clear(pendingRaw)
	pendingKeys := e.pendingKeys
	check(competing, "superseded")
	if !bytes.Equal(e.pendingRaw, pendingRaw) || e.pendingKeys != pendingKeys {
		t.Fatal("scope reconciliation changed staged vault/key successor")
	}
}

func TestBrowserScopeCandidateRetainsCurrentWriterAcrossCustodyAdvance(t *testing.T) {
	ctx := context.Background()
	e, initial := initialized(t)
	defer e.clear()
	prepared, err := e.run(ctx, request{Action: "set", Vault: initial, WorkspaceID: "personal", Kind: "personal", Owner: initial.Account, Name: "TOKEN", Value: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	envelope := prepared.(map[string]string)["envelope"]
	if _, err := e.run(ctx, request{Action: "password", Vault: initial, Password: "new fixture password"}); err != nil {
		t.Fatal(err)
	}
	next := stateFrom(t, e.pendingRaw)
	if _, err := e.run(ctx, request{Action: "commit", Vault: next}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.run(ctx, request{Action: "scope-candidate", Vault: next, WorkspaceID: "personal", Kind: "personal", Owner: next.Account, Envelope: envelope}); err != nil {
		t.Fatal("unchanged current signer rejected after custody advance", err)
	}
	future, err := env.SealVaultScope(ctx, env.VaultScopeClaims{Issuer: e.head.Issuer, OwnerKind: "personal", OwnerID: e.head.AccountID, WorkspaceID: "personal", KeyEpoch: e.keys.PersonalEpoch, Revision: 1, Previous: make([]byte, 32), WriterAccount: e.head.AccountID, WriterVaultGeneration: e.head.Generation + 1}, e.keys.PersonalKey, e.keys.WriterSeed, map[string][]byte{"TOKEN": []byte("fixture")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.run(ctx, request{Action: "scope-candidate", Vault: next, WorkspaceID: "personal", Kind: "personal", Owner: next.Account, Envelope: encoded(future.Raw)}); err == nil {
		t.Fatal("future custody generation accepted")
	}
}
