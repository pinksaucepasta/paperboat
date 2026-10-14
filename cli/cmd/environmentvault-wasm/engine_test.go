package main

import (
	"bytes"
	"context"
	"crypto/ecdh"
	env "github.com/pinksaucepasta/paperboat/internal/environmente2ee"
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

func createAnchor(t *testing.T, e *engine, v vaultState, workspace, kind, owner, machine string) scopeState {
	t.Helper()
	out, err := e.run(context.Background(), request{Action: "scope-create", Vault: v, WorkspaceID: workspace, Kind: kind, Owner: owner, Machine: machine})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := decode(out.(map[string]string)["envelope"])
	if err != nil {
		t.Fatal(err)
	}
	protection, err := env.PasswordVaultProtection(e.raw)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := env.ParseVaultScope(raw, protection.WriterPublic)
	if err != nil {
		t.Fatal(err)
	}
	c := scope.Claims
	return scopeState{WorkspaceID: c.WorkspaceID, WriterPublic: encoded(protection.WriterPublic), Kind: c.OwnerKind, Owner: c.OwnerID, Machine: c.MachineID, Epoch: c.KeyEpoch, Revision: c.Revision, DocumentID: digest(scope.ID), Envelope: encoded(raw)}
}

func writeRecord(t *testing.T, e *engine, v vaultState, anchor scopeState, name, value string, previous *recordState) recordState {
	t.Helper()
	idResult, err := e.run(context.Background(), request{Action: "record-id", Vault: v, WorkspaceID: anchor.WorkspaceID, Kind: anchor.Kind, Owner: anchor.Owner, Machine: anchor.Machine, Name: name})
	if err != nil {
		t.Fatal(err)
	}
	id := idResult.(map[string]string)["record_id"]
	out, err := e.run(context.Background(), request{Action: "record-write", Vault: v, Scope: &anchor, WorkspaceID: anchor.WorkspaceID, Kind: anchor.Kind, Owner: anchor.Owner, Machine: anchor.Machine, Name: name, Value: value, Record: previous})
	if err != nil {
		t.Fatal(err)
	}
	result := out.(map[string]any)
	wantExpectedRevision := uint64(0)
	if previous != nil {
		wantExpectedRevision = previous.Revision
	}
	if result["expected_revision"].(uint64) != wantExpectedRevision {
		t.Fatal("record write returned the wrong compare-and-swap revision")
	}
	raw, err := decode(result["envelope"].(string))
	if err != nil {
		t.Fatal(err)
	}
	writer, err := decode(anchor.WriterPublic)
	if err != nil {
		t.Fatal(err)
	}
	document, err := env.ParseVaultRecord(raw, writer)
	if err != nil {
		t.Fatal(err)
	}
	if document.Claims.RecordID != id {
		t.Fatal("record write used a different identifier than record-id")
	}
	c := document.Claims
	return recordState{WorkspaceID: c.WorkspaceID, Kind: c.OwnerKind, Owner: c.OwnerID, Machine: c.MachineID, Epoch: c.KeyEpoch, RecordID: c.RecordID, Revision: c.Revision, Deleted: c.Deleted, DocumentID: digest(document.ID), WriterPublic: anchor.WriterPublic, Envelope: encoded(raw)}
}

func readRecord(t *testing.T, e *engine, v vaultState, record recordState) (string, string, bool) {
	t.Helper()
	out, err := e.run(context.Background(), request{Action: "record-read", Vault: v, Record: &record})
	if err != nil {
		t.Fatal(err)
	}
	data := out.(map[string]any)
	return data["name"].(string), data["value"].(string), data["deleted"].(bool)
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
	anchor := createAnchor(t, e, initial, "personal", "personal", initial.Account, "")
	record := writeRecord(t, e, initial, anchor, "TOKEN", "private-value", nil)
	raw, _ := decode(anchor.Envelope)
	protection, _ := env.PasswordVaultProtection(e.raw)
	scope, err := env.ParseVaultScope(raw, protection.WriterPublic)
	if err != nil {
		t.Fatal(err)
	}
	values, err := env.OpenVaultScope(ctx, scope, e.keys.PersonalKey)
	if err != nil || len(values) != 0 {
		t.Fatalf("native empty anchor read failed: %v", err)
	}
	for _, v := range values {
		clear(v)
	}
	recordKey, err := env.VaultRecordKey(e.keys.PersonalKey, "personal", "personal", initial.Account, "", e.keys.PersonalEpoch)
	if err != nil {
		t.Fatal(err)
	}
	document, err := recordDocument(record)
	if err != nil {
		clear(recordKey)
		t.Fatal(err)
	}
	nativeName, nativeValue, err := env.OpenVaultRecord(ctx, document, recordKey)
	clear(recordKey)
	if err != nil || nativeName != "TOKEN" || string(nativeValue) != "private-value" {
		clear(nativeValue)
		t.Fatalf("native record read failed: %v", err)
	}
	clear(nativeValue)
	name, value, deleted := readRecord(t, e, initial, record)
	if name != "TOKEN" || value != "private-value" || deleted {
		t.Fatal("native record read returned incorrect value")
	}
	updatedRecord := writeRecord(t, e, initial, anchor, "TOKEN", "updated-value", &record)
	if updatedRecord.Revision != record.Revision+1 {
		t.Fatal("record update did not advance its revision")
	}
	if gotName, gotValue, wasDeleted := readRecord(t, e, initial, updatedRecord); gotName != "TOKEN" || gotValue != "updated-value" || wasDeleted {
		t.Fatal("updated record did not replace the prior value")
	}
	if _, err := e.run(ctx, request{Action: "record-id", Vault: initial, WorkspaceID: "personal", Kind: "personal", Owner: "other-account", Name: "TOKEN"}); err == nil {
		t.Fatal("cross account accepted")
	}
	if _, err := e.run(ctx, request{Action: "password", Vault: initial, Password: "replacement password"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.run(ctx, request{Action: "record-write", Vault: initial, Scope: &anchor, WorkspaceID: "personal", Kind: "personal", Owner: initial.Account, Name: "LATE", Value: "secret"}); err == nil {
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
	if _, err := e.run(ctx, request{Action: "record-id", Vault: initial, WorkspaceID: "personal", Kind: "personal", Owner: initial.Account, Name: "STALE"}); err == nil {
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
	teamRecord := writeRecord(t, e, next, team.Scope, "TEAM_TOKEN", "private", nil)
	team.Members[0].Membership = 2
	if _, _, err = e.teamKey(team, "read"); err == nil {
		t.Fatal("stale membership accepted")
	}
	team.Members[0].Membership = 1
	team.Members[0].Permission = "read"
	if _, _, err = e.teamKey(team, "write"); err == nil {
		t.Fatal("read-only mutation accepted")
	}
	if gotName, gotValue, deleted := readRecord(t, e, next, teamRecord); gotName != "TEAM_TOKEN" || gotValue != "private" || deleted {
		t.Fatal("team record did not retain its encrypted value")
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
	if _, err := e.run(ctx, request{Action: "record-id", Vault: v, WorkspaceID: "personal", Kind: "personal", Owner: v.Account, Name: "TOKEN"}); err == nil {
		t.Fatal("stale vault accepted after reset")
	}
	// Switching account must immediately erase unlocked key material.
	other := next
	other.Account = "account-two"
	if _, err := e.run(ctx, request{Action: "record-id", Vault: other, WorkspaceID: "personal", Kind: "personal", Owner: other.Account, Name: "TOKEN"}); err == nil {
		t.Fatal("account switch accepted")
	}
	if len(e.keys.PersonalKey) != 0 {
		t.Fatal("account switch retained keys")
	}
}
func TestBrowserPersonalRotationPreservesNativeRecords(t *testing.T) {
	ctx := context.Background()
	e, v := initialized(t)
	defer e.clear()
	anchor := createAnchor(t, e, v, "personal", "personal", v.Account, "")
	oldRecord := writeRecord(t, e, v, anchor, "TOKEN", "preserved", nil)
	oldRecord.Sequence = 1
	raw, _ := decode(anchor.Envelope)
	protection, _ := env.PasswordVaultProtection(e.raw)
	doc, err := env.ParseVaultScope(raw, protection.WriterPublic)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = e.run(ctx, request{Action: "personal-rotate", Vault: v, KeyEpoch: 1, Inventory: []scopeState{anchor}}); err != nil {
		t.Fatal(err)
	}
	out, err := e.run(ctx, request{Action: "personal-rotate-scope", Vault: v, Scope: &anchor, RecordSequence: 1, Records: []recordState{oldRecord}})
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
	if err != nil || len(values) != 0 {
		t.Fatal("rotation produced a non-empty anchor")
	}
	for _, value := range values {
		clear(value)
	}
	if nextDoc.Claims.KeyEpoch != 2 || nextDoc.Claims.Revision != 2 || nextDoc.Claims.WriterVaultGeneration != 2 {
		t.Fatal("rotation scope fencing incorrect")
	}
	replacement := out.(map[string]any)["records"].(recordReplacement)
	if replacement.ExpectedSequence != 1 || len(replacement.DocumentIDs) != 1 || replacement.DocumentIDs[0] != oldRecord.DocumentID || len(replacement.Envelopes) != 1 {
		t.Fatal("rotation did not return the expected record replacement")
	}
	nextRecordRaw, err := decode(replacement.Envelopes[0])
	if err != nil {
		t.Fatal(err)
	}
	nextRecordDoc, err := env.ParseVaultRecord(nextRecordRaw, nextProtection.WriterPublic)
	if err != nil {
		t.Fatal(err)
	}
	c := nextRecordDoc.Claims
	nextRecord := recordState{WorkspaceID: c.WorkspaceID, Kind: c.OwnerKind, Owner: c.OwnerID, Machine: c.MachineID, Epoch: c.KeyEpoch, RecordID: c.RecordID, Revision: c.Revision, Deleted: c.Deleted, Sequence: 1, DocumentID: digest(nextRecordDoc.ID), WriterPublic: encoded(nextProtection.WriterPublic), Envelope: encoded(nextRecordRaw)}
	next := stateFrom(t, e.pendingRaw)
	if _, err = e.run(ctx, request{Action: "commit", Vault: next}); err != nil {
		t.Fatal(err)
	}
	if name, value, deleted := readRecord(t, e, next, nextRecord); name != "TOKEN" || value != "preserved" || deleted {
		t.Fatal("rotation lost encrypted record value")
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
	teamRecord := writeRecord(t, alice, a, team.Scope, "SHARED_TOKEN", "shared value", nil)
	teamRecord.Sequence = 1
	team.RecordSequence = 1
	if _, err = bob.run(ctx, request{Action: "record-read", Vault: b, Record: &teamRecord}); err == nil {
		t.Fatal("team record opened before accepting its grant")
	}
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
	if name, value, deleted := readRecord(t, bob, b, teamRecord); name != "SHARED_TOKEN" || value != "shared value" || deleted {
		t.Fatal("accepted grant cannot open team record")
	}
	alicePublic, _ := ecdh.X25519().NewPrivateKey(alice.keys.SharingPrivate)
	out, err = alice.run(ctx, request{Action: "team-rotate", Vault: a, Team: &team, Owner: team.ID, Operation: "operation-rotate", Remove: []string{b.Account}, Sharing: []sharingState{{Account: a.Account, Generation: a.Generation, Public: encoded(alicePublic.PublicKey().Bytes())}}, RecordSequence: 1, Records: []recordState{teamRecord}})
	if err != nil {
		t.Fatal(err)
	}
	payload := out.(map[string]any)
	if len(payload["grant_envelopes"].([]string)) != 1 {
		t.Fatal("removed member received successor grant")
	}
	replacement := payload["records"].(recordReplacement)
	if replacement.ExpectedSequence != 1 || len(replacement.DocumentIDs) != 1 || replacement.DocumentIDs[0] != teamRecord.DocumentID || len(replacement.Envelopes) != 1 {
		t.Fatal("team rotation did not return the expected record replacement")
	}
	rotatedRaw, _ := decode(payload["scope_envelope"].(string))
	rotated, err := env.ParseVaultScope(rotatedRaw, writer.WriterPublic)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Claims.KeyEpoch != 2 {
		t.Fatal("rotation epoch not advanced")
	}
	rotatedRecordRaw, err := decode(replacement.Envelopes[0])
	if err != nil {
		t.Fatal(err)
	}
	rotatedRecord, err := env.ParseVaultRecord(rotatedRecordRaw, writer.WriterPublic)
	if err != nil {
		t.Fatal(err)
	}
	if rotatedRecord.Claims.KeyEpoch != 2 || rotatedRecord.Claims.Revision != 1 || rotatedRecord.Claims.RecordID == teamRecord.RecordID {
		t.Fatal("team rotation record fencing incorrect")
	}
	newKey, err := env.VaultRecordKey(alice.pendingKeys.Teams[0].Key, team.ID, "team", team.ID, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	name, value, err := env.OpenVaultRecord(ctx, rotatedRecord, newKey)
	clear(newKey)
	if err != nil || name != "SHARED_TOKEN" || string(value) != "shared value" {
		clear(value)
		t.Fatal("team rotation did not preserve encrypted record", err)
	}
	clear(value)
	oldKey, err := env.VaultRecordKey(bob.keys.Teams[0].Key, team.ID, "team", team.ID, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = env.OpenVaultRecord(ctx, rotatedRecord, oldKey); err == nil {
		clear(oldKey)
		t.Fatal("removed member opened successor record ciphertext")
	}
	clear(oldKey)
}
func TestBrowserRecordIdentifierRejectsReservedNames(t *testing.T) {
	e, v := initialized(t)
	defer e.clear()
	for _, name := range []string{"PAPERBOAT_API_AUDIT", "paperboat_token", "LD_PRELOAD", "DYLD_LIBRARY_PATH", "NODE_OPTIONS", "PYTHONPATH", "PYTHONHOME", "GOTRACEBACK", "1INVALID"} {
		_, err := e.run(context.Background(), request{Action: "record-id", Vault: v, WorkspaceID: "personal", Kind: "personal", Owner: v.Account, Name: name})
		if err == nil {
			t.Fatalf("reserved or invalid name %q was accepted", name)
		}
	}
	out, err := e.run(context.Background(), request{Action: "record-id", Vault: v, WorkspaceID: "personal", Kind: "personal", Owner: v.Account, Name: "PB_API_AUDIT"})
	if err != nil || out.(map[string]string)["record_id"] == "" {
		t.Fatal("valid variable name has no encrypted-record identifier", err)
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
	prepare := func(previous *scopeState) scopeState {
		t.Helper()
		claims := env.VaultScopeClaims{Issuer: e.head.Issuer, OwnerKind: "personal", OwnerID: e.head.AccountID, WorkspaceID: "personal", KeyEpoch: e.keys.PersonalEpoch, Revision: 1, Previous: make([]byte, 32), WriterAccount: e.head.AccountID, WriterVaultGeneration: e.head.Generation}
		if previous != nil {
			prior, err := scopeDocument(*previous)
			if err != nil {
				t.Fatal(err)
			}
			claims = prior.Claims
			previousID, err := env.ParseDocumentID(previous.DocumentID)
			if err != nil {
				t.Fatal(err)
			}
			claims.Revision++
			claims.Previous = previousID[:]
		}
		document, err := env.SealVaultScope(ctx, claims, e.keys.PersonalKey, e.keys.WriterSeed, map[string][]byte{})
		if err != nil {
			t.Fatal(err)
		}
		metadata, err := e.run(ctx, request{Action: "scope-candidate", Vault: v, Envelope: encoded(document.Raw), WorkspaceID: "personal", Kind: "personal", Owner: v.Account})
		if err != nil {
			t.Fatal(err)
		}
		return metadata.(scopeState)
	}
	first := createAnchor(t, e, v, "personal", "personal", v.Account, "")
	candidate := prepare(&first)
	competing := prepare(&first)
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
	newer := prepare(&candidate)
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
	prepared := createAnchor(t, e, initial, "personal", "personal", initial.Account, "")
	envelope := prepared.Envelope
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
	future, err := env.SealVaultScope(ctx, env.VaultScopeClaims{Issuer: e.head.Issuer, OwnerKind: "personal", OwnerID: e.head.AccountID, WorkspaceID: "personal", KeyEpoch: e.keys.PersonalEpoch, Revision: 1, Previous: make([]byte, 32), WriterAccount: e.head.AccountID, WriterVaultGeneration: e.head.Generation + 1}, e.keys.PersonalKey, e.keys.WriterSeed, map[string][]byte{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.run(ctx, request{Action: "scope-candidate", Vault: next, WorkspaceID: "personal", Kind: "personal", Owner: next.Account, Envelope: encoded(future.Raw)}); err == nil {
		t.Fatal("future custody generation accepted")
	}
}
