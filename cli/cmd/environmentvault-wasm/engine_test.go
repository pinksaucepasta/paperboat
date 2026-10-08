package main

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
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
	result, err := e.run(ctx, request{Action: "set", Vault: initial, Kind: "personal", Owner: initial.Account, Name: "TOKEN", Value: "private-value"})
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
	state := scopeState{WriterPublic: encoded(protection.WriterPublic), Kind: "personal", Owner: initial.Account, Epoch: scope.Claims.KeyEpoch, Revision: scope.Claims.Revision, DocumentID: digest(scope.ID), Envelope: encoded(raw)}
	names, err := e.run(ctx, request{Action: "names", Vault: initial, Scope: &state, Kind: "personal", Owner: initial.Account})
	if err != nil || names.(map[string]any)["names"].([]string)[0] != "TOKEN" {
		t.Fatalf("names failed: %v", err)
	}
	if _, err := e.run(ctx, request{Action: "names", Vault: initial, Scope: &state, Kind: "personal", Owner: "other-account"}); err == nil {
		t.Fatal("cross account accepted")
	}
	if _, err := e.run(ctx, request{Action: "password", Vault: initial, Password: "replacement password"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.run(ctx, request{Action: "set", Vault: initial, Kind: "personal", Owner: initial.Account, Name: "LATE", Value: "secret"}); err == nil {
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
	if _, err := e.run(ctx, request{Action: "set", Vault: initial, Kind: "personal", Owner: initial.Account, Name: "STALE", Value: "secret"}); err == nil {
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
	team.Scope = scopeState{Kind: "team", Owner: team.ID, Epoch: 1, Revision: 1, DocumentID: digest(scope.ID), Envelope: encoded(raw), WriterPublic: encoded(protection.WriterPublic)}
	_, err = e.run(ctx, request{Action: "set", Vault: next, Team: &team, Scope: &team.Scope, Kind: "team", Owner: team.ID, Name: "TEAM_TOKEN", Value: "private"})
	if err != nil {
		t.Fatal(err)
	}
	team.Members[0].Membership = 2
	if _, err = e.run(ctx, request{Action: "names", Vault: next, Team: &team, Scope: &team.Scope, Kind: "team", Owner: team.ID}); err == nil {
		t.Fatal("stale membership accepted")
	}
	team.Members[0].Membership = 1
	team.Members[0].Permission = "read"
	if _, err = e.run(ctx, request{Action: "set", Vault: next, Team: &team, Scope: &team.Scope, Kind: "team", Owner: team.ID, Name: "TOKEN", Value: "secret"}); err == nil {
		t.Fatal("read-only mutation accepted")
	}
}
func TestBrowserResetRejectsInvalidInventoryAndReplacesKeys(t *testing.T) {
	ctx := context.Background()
	e, v := initialized(t)
	defer e.clear()
	oldKey := append([]byte(nil), e.keys.PersonalKey...)
	defer clear(oldKey)
	bad := scopeState{Kind: "personal", Owner: "another-account", Epoch: 1, Revision: 1}
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
	if _, err := e.run(ctx, request{Action: "names", Vault: v, Kind: "personal", Owner: v.Account}); err == nil {
		t.Fatal("stale vault accepted after reset")
	}
	// Switching account must immediately erase unlocked key material.
	other := next
	other.Account = "account-two"
	if _, err := e.run(ctx, request{Action: "names", Vault: other, Kind: "personal", Owner: other.Account}); err == nil {
		t.Fatal("account switch accepted")
	}
	if len(e.keys.PersonalKey) != 0 {
		t.Fatal("account switch retained keys")
	}
}
func TestBrowserHostProjectionExplicitSelectionAndAuthority(t *testing.T) {
	ctx := context.Background()
	e, v := initialized(t)
	defer e.clear()
	makeScope := func(machine string, values map[string]string) scopeState {
		t.Helper()
		var state *scopeState
		for name, value := range values {
			out, err := e.run(ctx, request{Action: "set", Vault: v, Scope: state, Kind: "personal", Owner: v.Account, Machine: machine, Name: name, Value: value})
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := decode(out.(map[string]string)["envelope"])
			protection, _ := env.PasswordVaultProtection(e.raw)
			doc, err := env.ParseVaultScope(raw, protection.WriterPublic)
			if err != nil {
				t.Fatal(err)
			}
			state = &scopeState{Kind: "personal", Owner: v.Account, Machine: machine, Epoch: doc.Claims.KeyEpoch, Revision: doc.Claims.Revision, DocumentID: digest(doc.ID), Envelope: encoded(raw), WriterPublic: encoded(protection.WriterPublic)}
		}
		return *state
	}
	base := makeScope("", map[string]string{"SELECTED": "base", "OMITTED": "hidden"})
	override := makeScope("machine-one", map[string]string{"SELECTED": "override"})
	hostKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	host := hostState{Bundle: env.ProjectionBundle{AccountID: v.Account, MachineID: "machine-one", InstallationGeneration: 1, HostKeyGeneration: 1, HostPublic: encoded(hostKey.PublicKey().Bytes()), State: "ready"}}
	request := request{Action: "host-provision", Vault: v, Host: &host, Machine: "machine-one", Operation: "operation-host", Inventory: []scopeState{base, override}, Selection: []hostSelection{{Kind: "personal", Owner: v.Account, Name: "SELECTED"}}}
	out, err := e.run(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := decode(out.(map[string]any)["envelope"].(string))
	protection, _ := env.PasswordVaultProtection(e.raw)
	projection, err := env.ParseHostProjection(raw, protection.WriterPublic)
	if err != nil {
		t.Fatal(err)
	}
	values, err := env.OpenHostProjection(ctx, projection, projection.Claims, hostKey.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, v := range values {
			clear(v)
		}
	}()
	if len(values) != 1 || string(values["SELECTED"]) != "override" {
		t.Fatal("selection or override contract violated")
	}
	host.Bundle.AccountID = "another-account"
	if _, err = e.run(ctx, request); err == nil {
		t.Fatal("foreign installation accepted")
	}
	host.Bundle.AccountID = v.Account
	host.Bundle.State = "revoked"
	if _, err = e.run(ctx, request); err == nil {
		t.Fatal("revoked installation accepted")
	}
}
func TestBrowserPersonalRotationPreservesNativeValues(t *testing.T) {
	ctx := context.Background()
	e, v := initialized(t)
	defer e.clear()
	out, err := e.run(ctx, request{Action: "set", Vault: v, Kind: "personal", Owner: v.Account, Name: "TOKEN", Value: "preserved"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := decode(out.(map[string]string)["envelope"])
	protection, _ := env.PasswordVaultProtection(e.raw)
	doc, err := env.ParseVaultScope(raw, protection.WriterPublic)
	if err != nil {
		t.Fatal(err)
	}
	scope := scopeState{Kind: "personal", Owner: v.Account, Epoch: 1, Revision: 1, DocumentID: digest(doc.ID), WriterPublic: encoded(protection.WriterPublic), Envelope: encoded(raw)}
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
	team.Scope = scopeState{Kind: "team", Owner: team.ID, Epoch: 1, Revision: 1, DocumentID: digest(scope.ID), Envelope: encoded(raw), WriterPublic: encoded(writer.WriterPublic)}
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
	if _, err = bob.run(ctx, request{Action: "names", Vault: b, Kind: "team", Owner: team.ID, Team: &team, Scope: &team.Scope}); err != nil {
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
		_, err := e.run(context.Background(), request{Action: "set", Vault: v, Kind: "personal", Owner: v.Account, Name: name, Value: "private"})
		if err == nil || !strings.Contains(err.Error(), "variable name is invalid or reserved") {
			t.Fatalf("%s has no actionable name error", name)
		}
	}
	if _, err := e.run(context.Background(), request{Action: "set", Vault: v, Kind: "personal", Owner: v.Account, Name: "PB_API_AUDIT", Value: "private"}); err != nil {
		t.Fatal(err)
	}
}
