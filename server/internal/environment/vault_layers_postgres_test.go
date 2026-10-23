package environment

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/config"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/teams"
)

// Uses the real v1 CBOR claims and signature transcript, not unsigned delivery
// rows. Ciphertext is a generated fixture; the storage boundary stays opaque.
func signedVaultLayerFixture(t *testing.T, source VaultScopeState, recipient VaultLayerRecipient, writer PasswordVaultMetadata) []byte {
	t.Helper()
	digest := func(id string) []byte {
		t.Helper()
		if id == "" {
			return make([]byte, 32)
		}
		raw, err := hex.DecodeString(strings.TrimPrefix(id, "sha256:"))
		if err != nil || len(raw) != 32 {
			t.Fatal("invalid fixture document digest")
		}
		return raw
	}
	public, err := DecodeCanonicalBase64URL(recipient.HostPublic, 32)
	if err != nil {
		t.Fatal(err)
	}
	claims := vaultLayerClaims{Domain: "paperboat.environment.host-layer", Version: 1, Issuer: "https://control.example", RecipientAccount: recipient.RecipientAccount, MachineID: recipient.MachineID, InstallationGeneration: recipient.InstallationGeneration, HostKeyGeneration: recipient.HostKeyGeneration, HostPublic: public, DeliveryGeneration: recipient.DeliveryGeneration + 1, Previous: digest(recipient.DocumentID), FenceGeneration: recipient.FenceGeneration, Source: vaultLayerSourceClaims{WorkspaceID: source.WorkspaceID, OwnerKind: source.OwnerKind, OwnerID: source.OwnerID, MachineID: source.MachineID, KeyEpoch: source.KeyEpoch, Revision: source.Revision, Digest: digest(source.DocumentID)}, WriterAccount: writer.Head.AccountID, WriterVaultGeneration: writer.Head.Generation, WriterPublic: writer.WriterPublicKey[:]}
	encoded, err := EncodeCanonical(claims)
	if err != nil {
		t.Fatal(err)
	}
	enc, ciphertext := bytes.Repeat([]byte{4}, 32), bytes.Repeat([]byte{5}, 48)
	transcript, err := EncodeCanonical([]any{claims.Domain + "-signature", uint64(1), encoded, enc, ciphertext})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := EncodeCanonical(vaultLayerEnvelope{Claims: encoded, Enc: enc, Ciphertext: ciphertext, Signature: ed25519.Sign(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{11}, 32)), transcript)})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestPostgresVaultLayersFanoutSignedPublicationAndWorkspaceIsolation(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("PAPERBOAT_TEST_DATABASE_DSN"))
	if dsn == "" {
		t.Skip("set isolated PAPERBOAT_TEST_DATABASE_DSN for signed ENV layer fanout")
	}
	if err := db.ValidateIsolatedTestDSN(dsn, os.Getenv("PAPERBOAT_DATABASE_DSN")); err != nil {
		t.Fatal(err)
	}
	store, err := db.Open(config.Database{Driver: "postgres", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	setup, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	err = db.Migrate(setup, store)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	// This full signed lifecycle crosses the remote DB link for each bounded
	// transaction; keep its fixture budget separate from production deadlines.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	suffix := strings.ReplaceAll(time.Now().UTC().Format("20060102150405.000000000"), ".", "")
	owner, member, noVault, outsider := "vl_owner_"+suffix, "vl_member_"+suffix, "vl_novault_"+suffix, "vl_outside_"+suffix
	team, otherTeam := "vl_team_"+suffix, "vl_other_"+suffix
	ownerA, ownerB, memberHost, noVaultHost, outsideHost := "vl_oa_"+suffix, "vl_ob_"+suffix, "vl_m_"+suffix, "vl_n_"+suffix, "vl_x_"+suffix
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		err := store.InTx(cleanup, func(ctx context.Context, tx *db.Tx) error {
			if _, err := tx.Exec(ctx, `ALTER TABLE audit_events DISABLE TRIGGER audit_events_append_only`); err != nil {
				return err
			}
			// Removed Team machines retain their ownership reference; delete
			// only this fixture's machines before deleting its Teams.
			if _, err := tx.Exec(ctx, `DELETE FROM user_machines WHERE id=ANY($1::text[]) AND user_id=ANY($2::text[])`, []string{ownerA, ownerB, memberHost, noVaultHost, outsideHost}, []string{owner, member, noVault, outsider}); err != nil {
				return err
			}
			queries := []string{`DELETE FROM audit_events WHERE resource_id IN($5,$6) AND actor_user_id IN($1,$2,$3,$4)`, `DELETE FROM environment_vault_scopes WHERE owner_id IN($1,$2,$3,$4,$5,$6)`, `DELETE FROM billing_workspaces WHERE team_id IN($5,$6) AND $1<>'' AND $2<>'' AND $3<>'' AND $4<>''`, `DELETE FROM teams WHERE team_id IN($5,$6) AND owner_account=$1 AND $2<>'' AND $3<>'' AND $4<>''`, `DELETE FROM users WHERE id IN($1,$2,$3,$4) AND $5<>'' AND $6<>''`}
			for _, q := range queries {
				if _, err := tx.Exec(ctx, q, owner, member, noVault, outsider, team, otherTeam); err != nil {
					return err
				}
			}
			_, err := tx.Exec(ctx, `ALTER TABLE audit_events ENABLE TRIGGER audit_events_append_only`)
			return err
		})
		if err != nil {
			t.Errorf("cleanup exact layer fixture: %v", err)
		}
	}()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := store.SQL().ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, account := range []string{owner, member, noVault, outsider} {
		exec(`INSERT INTO users(id,workos_subject,primary_email,status) VALUES($1,$1,$1||'@invalid.test','active')`, account)
	}
	hosts := map[string]string{ownerA: owner, ownerB: owner, memberHost: member, noVaultHost: noVault, outsideHost: outsider}
	for machine, account := range hosts {
		exec(`INSERT INTO user_machines(id,user_id,environment_id,platform,architecture,workspace_root,state,seat_state,configured_capabilities) VALUES($1,$2,$3,'linux','amd64','/vault-layer-test','online','released',ARRAY['environment_injection'])`, machine, account, "env_"+machine)
	}
	service := NewService(store, nil, nil, "https://control.example")
	vaults := map[string]PasswordVaultMetadata{}
	rawVaults := map[string][]byte{}
	for _, account := range []string{owner, member} {
		raw := passwordVaultFixture(t, "https://control.example", account, 1, make([]byte, 32))
		if _, err := service.PutPasswordVault(ctx, account, raw); err != nil {
			t.Fatal(err)
		}
		metadata, err := ParsePasswordVaultMetadata(raw, "https://control.example", account)
		if err != nil {
			t.Fatal(err)
		}
		vaults[account] = metadata
		rawVaults[account] = raw
	}
	for machine, account := range hosts {
		recipient, err := service.RegisterVaultHostKey(ctx, account, machine, VaultHostKeyRequest{OperationID: "register_" + machine, InstallationGeneration: 1, HostKeyGeneration: 1, HostPublic: b64VaultPublic(bytes.Repeat([]byte{19}, 32))})
		if err != nil || recipient.MachineID != machine {
			t.Fatalf("register host %s: %v", machine, err)
		}
	}
	operation := 0
	nextOperation := func() string { operation++; return "vl_op_" + suffix + "_" + strconv.Itoa(operation) }
	putScope := func(account string, c VaultLayerCoordinate, revision uint64, previous []byte) VaultScopeState {
		t.Helper()
		h := fixtureScopeHeader(c.OwnerID, c.OwnerKind, account, vaults[account].Head.Generation, 1, revision, previous)
		h.WorkspaceID = c.WorkspaceID
		h.MachineID = c.MachineID
		out, err := service.PutVaultScope(ctx, account, c.WorkspaceID, c.OwnerKind, c.OwnerID, c.MachineID, VaultScopePut{OperationID: nextOperation(), Envelope: b64Vault(vaultScopeFixture(t, h, 11))})
		if err != nil {
			t.Fatal("put signed scope", err)
		}
		return out
	}
	// Context assertions exercise assigned ENV, not anchor-only scopes. Store a
	// real signed opaque record through the same write authority as production.
	assign := func(account string, source VaultScopeState) {
		t.Helper()
		c := VaultLayerCoordinate{WorkspaceID: source.WorkspaceID, OwnerKind: source.OwnerKind, OwnerID: source.OwnerID, MachineID: source.MachineID}
		envelope := vaultRecordFixture(t, c, account, strings.Repeat("1", 64), 1, source.KeyEpoch, false, vaults[account].Head.Generation)
		if _, err := service.PutVaultRecords(ctx, account, c, VaultRecordsPut{OperationID: nextOperation(), Records: []VaultRecordMutation{{Envelope: envelope}}}); err != nil {
			t.Fatal("assign signed scope record", err)
		}
	}
	personal := VaultLayerCoordinate{WorkspaceID: "personal", OwnerKind: "personal", OwnerID: owner}
	global := putScope(owner, personal, 1, make([]byte, 32))
	device := personal
	device.MachineID = ownerA
	deviceSource := putScope(owner, device, 1, make([]byte, 32))
	assign(owner, global)
	assign(owner, deviceSource)
	list := func(account string, c VaultLayerCoordinate, state string, limit, offset int, wantTotal int) []VaultLayerRecipient {
		t.Helper()
		items, total, err := service.VaultLayerRecipients(ctx, account, c, state, limit, offset)
		if err != nil || total != wantTotal {
			t.Fatalf("recipient %s %s offset %d: total=%d want=%d err=%v", c.WorkspaceID, state, offset, total, wantTotal, err)
		}
		return items
	}
	first := list(owner, personal, "pending", 1, 0, 2)
	second := list(owner, personal, "pending", 1, 1, 2)
	if len(first) != 1 || len(second) != 1 || first[0].MachineID == second[0].MachineID || first[0].MachineID > second[0].MachineID {
		t.Fatal("global recipient pages are incomplete or unstable")
	}
	if len(list(owner, personal, "pending", 1, 2, 2)) != 0 {
		t.Fatal("offset beyond last page returned recipient")
	}
	onlyDevice := list(owner, device, "pending", 20, 0, 1)
	if len(onlyDevice) != 1 || onlyDevice[0].MachineID != ownerA {
		t.Fatal("device scope fanout escaped exact device")
	}
	publish := func(account string, source VaultScopeState, r VaultLayerRecipient) (VaultLayerDelivery, VaultLayerPut) {
		t.Helper()
		request := VaultLayerPut{OperationID: nextOperation(), Envelope: b64Vault(signedVaultLayerFixture(t, source, r, vaults[account]))}
		out, err := service.PutVaultLayer(ctx, account, source.WorkspaceID, r.MachineID, request)
		if err != nil || out.State != "ready" || out.Applied {
			t.Fatalf("signed publication state=%s applied=%t err=%v", out.State, out.Applied, err)
		}
		return out, request
	}
	_, receipt := publish(owner, global, first[0])
	publish(owner, global, second[0])
	publish(owner, deviceSource, onlyDevice[0])
	list(owner, personal, "pending", 20, 0, 0)
	if len(list(owner, personal, "ready", 20, 0, 2)) != 2 {
		t.Fatal("publication omitted ready recipients")
	}
	if _, err := service.PutVaultLayer(ctx, owner, "personal", first[0].MachineID, receipt); err != nil {
		t.Fatal("exact layer retry failed", err)
	}
	bad := receipt
	bad.OperationID = nextOperation()
	raw, _ := DecodeCanonicalBase64URL(bad.Envelope, MaximumVaultLayerBytes)
	var envelope vaultLayerEnvelope
	_ = strictDecoding.Unmarshal(raw, &envelope)
	envelope.Signature[0] ^= 1
	changed, _ := EncodeCanonical(envelope)
	bad.Envelope = b64Vault(changed)
	if _, err := service.PutVaultLayer(ctx, owner, "personal", first[0].MachineID, bad); !errors.Is(err, ErrProtocolSignature) {
		t.Fatalf("tampered publication: %v", err)
	}
	current := list(owner, personal, "ready", 20, 0, 2)[0]
	staleRequest := VaultLayerPut{OperationID: nextOperation(), Envelope: b64Vault(signedVaultLayerFixture(t, global, current, vaults[owner]))}
	previous, _ := hex.DecodeString(strings.TrimPrefix(global.DocumentID, "sha256:"))
	global = putScope(owner, personal, 2, previous)
	list(owner, personal, "pending", 20, 0, 2)
	list(owner, personal, "ready", 20, 0, 0)
	if _, err := service.PutVaultLayer(ctx, owner, "personal", current.MachineID, staleRequest); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale source/fence publication: %v", err)
	}
	if _, err := service.PutVaultLayer(ctx, owner, "personal", first[0].MachineID, receipt); err != nil {
		t.Fatal("ACK-lost exact replay after source edit", err)
	}
	for _, r := range list(owner, personal, "pending", 20, 0, 2) {
		publish(owner, global, r)
	}
	for machine, want := range map[string]int{ownerA: 2, ownerB: 1} {
		launch, err := service.VaultLayerContext(ctx, owner, machine, owner, "personal")
		if err != nil || len(launch.Layers) != want {
			t.Fatalf("Personal global/device context layers=%d want=%d err=%v", len(launch.Layers), want, err)
		}
		for _, layer := range launch.Layers {
			if layer.State != "ready" || layer.Source.OwnerID != owner || layer.Source.WorkspaceID != "personal" {
				t.Fatal("Personal context returned wrong source or pending publication")
			}
		}
	}
	// Initialize Team through its signed vault/key-grant transaction. Ordinary
	// active members need no Personal vault or individual Team key to receive
	// opaque shared ciphertext from an authorized Team writer.
	old := rawVaults[owner]
	sum := sha256.Sum256(old)
	next := passwordVaultFixtureByte(t, "https://control.example", owner, 2, sum[:], 9)
	nextMetadata, err := ParsePasswordVaultMetadata(next, "https://control.example", owner)
	if err != nil {
		t.Fatal(err)
	}
	create := VaultTeamCreate{OperationID: nextOperation(), TeamID: team, ExpectedTeamGeneration: 1, VaultEnvelope: b64Vault(next), ScopeEnvelope: b64Vault(vaultScopeFixture(t, fixtureScopeHeader(team, "team", owner, 1, 1, 1, make([]byte, 32)), 11))}
	create.GrantEnvelope = b64Vault(vaultGrantFixture(t, fixtureGrantClaims(team, create.OperationID, 1, 1, vaults[owner], nextMetadata), 11))
	exec(`INSERT INTO teams(team_id,owner_account,generation) VALUES($1,$2,1)`, team, owner)
	exec(`INSERT INTO team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true)`, team, owner)
	exec(`INSERT INTO billing_workspaces(id,team_id,state,period_start,period_end) VALUES($1,$2,'active',now()-interval '1 hour',now()+interval '1 hour')`, "billing_"+team, team)
	if err := store.SQL().QueryRowContext(ctx, `SELECT generation FROM teams WHERE team_id=$1`, team).Scan(&create.ExpectedTeamGeneration); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateVaultTeam(ctx, owner, create); err != nil {
		t.Fatal("initialize signed Team", err)
	}
	vaults[owner] = nextMetadata
	for _, account := range []string{member, noVault} {
		exec(`INSERT INTO team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true)`, team, account)
	}
	sharedCoordinate := VaultLayerCoordinate{WorkspaceID: team, OwnerKind: "team", OwnerID: team}
	sharedSource, err := service.GetVaultScope(ctx, owner, team, "team", team, "")
	if err != nil {
		t.Fatal(err)
	}
	assign(owner, sharedSource)
	sharedRecipients := list(owner, sharedCoordinate, "pending", 2, 0, 4)
	sharedRecipients = append(sharedRecipients, list(owner, sharedCoordinate, "pending", 2, 2, 4)...)
	if len(sharedRecipients) != 4 {
		t.Fatal("Team recipient fanout silently capped")
	}
	sawNoVault := false
	var memberSharedReceipt VaultLayerPut
	for _, r := range sharedRecipients {
		if r.MachineID == outsideHost {
			t.Fatal("outsider in Team fanout")
		}
		sawNoVault = sawNoVault || r.MachineID == noVaultHost
		_, publishedRequest := publish(owner, sharedSource, r)
		if r.MachineID == memberHost {
			memberSharedReceipt = publishedRequest
		}
	}
	list(owner, sharedCoordinate, "pending", 20, 0, 0)
	list(owner, sharedCoordinate, "ready", 20, 0, 4)
	if !sawNoVault {
		t.Fatal("member without Personal vault omitted")
	}
	private := VaultLayerCoordinate{WorkspaceID: team, OwnerKind: "personal", OwnerID: member}
	privateSource := putScope(member, private, 1, make([]byte, 32))
	assign(member, privateSource)
	privateDevice := private
	privateDevice.MachineID = memberHost
	// A concurrent accepted Team capsule/password rewrap advances custody but
	// retains the current writer. Previously prepared ciphertext remains valid.
	h := fixtureScopeHeader(member, "personal", member, vaults[member].Head.Generation, 1, 1, make([]byte, 32))
	h.WorkspaceID, h.MachineID = team, memberHost
	preparedScope := VaultScopePut{OperationID: nextOperation(), Envelope: b64Vault(vaultScopeFixture(t, h, 11))}
	privateRecipient := list(member, private, "pending", 20, 0, 1)[0]
	preparedLayer := VaultLayerPut{OperationID: nextOperation(), Envelope: b64Vault(signedVaultLayerFixture(t, privateSource, privateRecipient, vaults[member]))}
	memberPrevious := sha256.Sum256(rawVaults[member])
	memberNext := passwordVaultFixture(t, "https://control.example", member, 2, memberPrevious[:])
	if _, err := service.PutPasswordVault(ctx, member, memberNext); err != nil {
		t.Fatal("same-writer custody advance", err)
	}
	vaults[member], err = ParsePasswordVaultMetadata(memberNext, "https://control.example", member)
	if err != nil {
		t.Fatal(err)
	}
	privateDeviceSource, err := service.PutVaultScope(ctx, member, team, "personal", member, memberHost, preparedScope)
	if err != nil {
		t.Fatal("prepared scope after same-writer custody advance", err)
	}
	assign(member, privateDeviceSource)
	if _, err := service.PutVaultScope(ctx, member, team, "personal", member, memberHost, preparedScope); err != nil {
		t.Fatal("prepared scope exact retry", err)
	}
	if _, err := service.PutVaultLayer(ctx, member, team, memberHost, preparedLayer); err != nil {
		t.Fatal("prepared layer after same-writer custody advance", err)
	}
	if _, err := service.PutVaultLayer(ctx, member, team, memberHost, preparedLayer); err != nil {
		t.Fatal("prepared layer exact retry", err)
	}
	for _, source := range []VaultScopeState{privateDeviceSource} {
		c := VaultLayerCoordinate{WorkspaceID: source.WorkspaceID, OwnerKind: source.OwnerKind, OwnerID: source.OwnerID, MachineID: source.MachineID}
		recipients := list(member, c, "pending", 20, 0, 1)
		if recipients[0].MachineID != memberHost {
			t.Fatal("member private scope escaped owner device")
		}
		publish(member, source, recipients[0])
	}
	if _, _, err := service.VaultLayerRecipients(ctx, owner, private, "", 20, 0); !errors.Is(err, ErrKeyAuthorizationRequired) {
		t.Fatalf("Team owner read member private inventory: %v", err)
	}
	contextFor := func(machineOwner, machine, actor, workspace string, want int) VaultLayerContext {
		t.Helper()
		out, err := service.VaultLayerContext(ctx, machineOwner, machine, actor, workspace)
		if err != nil || len(out.Layers) != want {
			t.Fatalf("context actor=%s workspace=%s layers=%d want=%d err=%v", actor, workspace, len(out.Layers), want, err)
		}
		for _, layer := range out.Layers {
			if layer.Source.WorkspaceID != workspace {
				t.Fatal("layer crossed workspace")
			}
		}
		return out
	}
	ownContext := contextFor(member, memberHost, member, team, 3)
	if ownContext.Layers[0].Source.OwnerKind != "team" || ownContext.Layers[1].Source.OwnerID != member || ownContext.Layers[2].Source.MachineID != memberHost {
		t.Fatal("three-layer hierarchy changed")
	}
	sharedOnly := contextFor(member, memberHost, owner, team, 1)
	if sharedOnly.Layers[0].Source.OwnerKind != "team" {
		t.Fatal("foreign actor inherited target member private ENV")
	}
	contextFor(noVault, noVaultHost, noVault, team, 1)
	contextFor(member, memberHost, member, "personal", 0)
	if _, err := service.VaultLayerContext(ctx, member, memberHost, owner, "personal"); !errors.Is(err, ErrKeyAuthorizationRequired) {
		t.Fatalf("foreign actor Personal launch: %v", err)
	}
	if _, err := service.VaultLayerContext(ctx, member, memberHost, outsider, team); !errors.Is(err, ErrKeyAuthorizationRequired) {
		t.Fatalf("nonmember Team launch: %v", err)
	}
	exec(`INSERT INTO teams(team_id,owner_account,generation) VALUES($1,$2,1)`, otherTeam, owner)
	for _, account := range []string{owner, member} {
		exec(`INSERT INTO team_members(team_id,account_id,membership_generation,role,active) VALUES($1,$2,1,'member',true)`, otherTeam, account)
	}
	exec(`INSERT INTO billing_workspaces(id,team_id,state,period_start,period_end) VALUES($1,$2,'active',now()-interval '1 hour',now()+interval '1 hour')`, "billing_"+otherTeam, otherTeam)
	contextFor(member, memberHost, member, otherTeam, 0)
	exec(`UPDATE team_members SET active=false WHERE team_id=$1 AND account_id=$2`, team, member)
	if _, err := service.VaultLayerContext(ctx, member, memberHost, member, team); !errors.Is(err, ErrKeyAuthorizationRequired) {
		t.Fatalf("revoked membership launch: %v", err)
	}
	if _, err := service.PutVaultLayer(ctx, owner, team, memberHost, memberSharedReceipt); !errors.Is(err, ErrKeyAuthorizationRequired) {
		t.Fatalf("revoked recipient exact replay: %v", err)
	}
	remaining := list(owner, sharedCoordinate, "", 20, 0, 3)
	for _, r := range remaining {
		if r.MachineID == memberHost {
			t.Fatal("revoked member retained Team fanout")
		}
	}
	// The keyless delivery assertions above remain intact. Give the second
	// surviving member real writer authority before exercising removal.
	secondWriterRaw := passwordVaultFixture(t, "https://control.example", noVault, 1, make([]byte, 32))
	if _, err := service.PutPasswordVault(ctx, noVault, secondWriterRaw); err != nil {
		t.Fatal(err)
	}
	vaults[noVault], err = ParsePasswordVaultMetadata(secondWriterRaw, "https://control.example", noVault)
	if err != nil {
		t.Fatal(err)
	}
	authority := teams.NewService(store)
	writerTeam, err := authority.Get(ctx, owner, team)
	if err != nil {
		t.Fatal(err)
	}
	writerTeam, err = authority.Grant(ctx, owner, team, teams.GrantRequest{OperationID: nextOperation(), ExpectedGeneration: writerTeam.Generation, AccountID: noVault, ResourceKind: "env", ResourceID: team, Permission: "write", Active: true})
	if err != nil {
		t.Fatal("authorize second survivor ENV writer", err)
	}
	writerGrant := VaultMemberGrant{OperationID: nextOperation(), AccountID: noVault, ExpectedTeamGeneration: writerTeam.Generation, ExpectedMembershipGeneration: 1}
	writerGrant.GrantEnvelope = b64Vault(vaultGrantFixture(t, fixtureGrantClaims(team, writerGrant.OperationID, 1, 1, vaults[owner], vaults[noVault]), 11))
	if _, err := service.GrantVaultTeamMember(ctx, owner, team, writerGrant); err != nil {
		t.Fatal("grant second survivor Team key", err)
	}
	if _, err := service.GetVaultScope(ctx, noVault, team, "team", team, ""); err != nil {
		t.Fatal("second survivor read baseline", err)
	}
	// Capture a valid publication before removal; it must not cross the
	// delivery fence even when addressed to the surviving machine.
	before := list(owner, personal, "ready", 20, 0, 2)
	var survivor, departing VaultLayerRecipient
	for _, r := range before {
		if r.MachineID == ownerA {
			survivor = r
		} else {
			departing = r
		}
	}
	staleSurvivor := VaultLayerPut{OperationID: nextOperation(), Envelope: b64Vault(signedVaultLayerFixture(t, global, survivor, vaults[owner]))}
	staleDeparting := VaultLayerPut{OperationID: nextOperation(), Envelope: b64Vault(signedVaultLayerFixture(t, global, departing, vaults[owner]))}
	currentTeam, err := authority.Get(ctx, owner, team)
	if err != nil {
		t.Fatal(err)
	}
	transferred, err := authority.Machine(ctx, owner, team, teams.MachineRequest{OperationID: nextOperation(), ExpectedGeneration: currentTeam.Generation, MachineID: ownerB, Action: "transfer_to_team", Confirmation: ownerB})
	if err != nil {
		t.Fatal("transfer recipient to Team", err)
	}
	var fenceBefore uint64
	if err := store.SQL().QueryRowContext(ctx, `SELECT generation FROM environment_vault_layer_fences WHERE workspace_id=$1 AND owner_kind='team' AND owner_id=$1 AND source_machine_id=''`, team).Scan(&fenceBefore); err != nil {
		t.Fatal(err)
	}
	removed, err := authority.Machine(ctx, owner, team, teams.MachineRequest{OperationID: nextOperation(), ExpectedGeneration: transferred.Generation, MachineID: ownerB, Action: "remove", Confirmation: ownerB})
	if err != nil {
		t.Fatal("remove Team recipient", err)
	}
	var fenceAfter uint64
	if err := store.SQL().QueryRowContext(ctx, `SELECT generation FROM environment_vault_layer_fences WHERE workspace_id=$1 AND owner_kind='team' AND owner_id=$1 AND source_machine_id=''`, team).Scan(&fenceAfter); err != nil {
		t.Fatal(err)
	}
	if fenceAfter != fenceBefore+1 || removed.Generation <= transferred.Generation {
		t.Fatalf("Team removal did not advance delivery authority: fence=%d/%d generation=%d/%d", fenceBefore, fenceAfter, transferred.Generation, removed.Generation)
	}
	if removed.ENVStatus != "ready" {
		t.Fatalf("Team machine removal ENV status=%s", removed.ENVStatus)
	}
	if _, err := service.PutVaultLayer(ctx, owner, "personal", ownerA, staleSurvivor); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("pre-removal survivor delivery crossed fence: %v", err)
	}
	if _, err := service.PutVaultLayer(ctx, owner, "personal", ownerB, staleDeparting); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removed host accepted delivery: %v", err)
	}
	if _, err := service.VaultLayerContext(ctx, owner, ownerB, owner, "personal"); !errors.Is(err, ErrMachineNotFound) {
		t.Fatalf("removed host retained ENV context: %v", err)
	}
	inventory, err := service.GetVaultPersonalScopes(ctx, owner)
	if err != nil || inventory.RotationRequired || inventory.KeyEpoch != 1 {
		t.Fatalf("removal changed Personal keys: %+v err=%v", inventory, err)
	}
	if _, err := service.GetVaultScope(ctx, owner, "personal", "personal", owner, ""); err != nil {
		t.Fatal("survivor Personal read", err)
	}
	previous, _ = hex.DecodeString(strings.TrimPrefix(global.DocumentID, "sha256:"))
	global = putScope(owner, personal, 3, previous)
	for _, r := range list(owner, personal, "pending", 20, 0, 1) {
		publish(owner, global, r)
	}
	// Removing a machine fences only the sources it could read. ownerB never received ownerA's device
	// source, so that delivery stays current and nothing is pending for it.
	if pending := list(owner, device, "pending", 20, 0, 0); len(pending) != 0 {
		t.Fatalf("removal fenced a source the removed machine could not read: %+v", pending)
	}
	contextFor(owner, ownerA, owner, "personal", 2)
	teamState, err := service.GetVaultTeam(ctx, owner, team)
	if err != nil || teamState.RotationRequired || teamState.KeyEpoch != 1 || teamState.Generation != removed.Generation {
		t.Fatalf("removal changed Team keys: %+v err=%v", teamState, err)
	}
	previous, _ = hex.DecodeString(strings.TrimPrefix(sharedSource.DocumentID, "sha256:"))
	sharedHeader := fixtureScopeHeader(team, "team", owner, vaults[owner].Head.Generation, 1, sharedSource.Revision+1, previous)
	sharedSource, err = service.PutVaultScope(ctx, owner, team, "team", team, "", VaultScopePut{OperationID: nextOperation(), Envelope: b64Vault(vaultScopeFixture(t, sharedHeader, 11))})
	if err != nil {
		t.Fatal("survivor Team write", err)
	}
	for _, r := range list(owner, sharedCoordinate, "pending", 20, 0, 2) {
		publish(owner, sharedSource, r)
	}
	if _, err := service.GetVaultScope(ctx, noVault, team, "team", team, ""); err != nil {
		t.Fatal("second survivor Team read after removal", err)
	}
	previous, _ = hex.DecodeString(strings.TrimPrefix(sharedSource.DocumentID, "sha256:"))
	secondHeader := fixtureScopeHeader(team, "team", noVault, vaults[noVault].Head.Generation, 1, sharedSource.Revision+1, previous)
	sharedSource, err = service.PutVaultScope(ctx, noVault, team, "team", team, "", VaultScopePut{OperationID: nextOperation(), Envelope: b64Vault(vaultScopeFixture(t, secondHeader, 11))})
	if err != nil {
		t.Fatal("second survivor Team write after removal", err)
	}
	for _, r := range list(noVault, sharedCoordinate, "pending", 20, 0, 2) {
		publish(noVault, sharedSource, r)
	}
	contextFor(owner, ownerA, owner, team, 1)
	contextFor(noVault, noVaultHost, noVault, team, 1)
}
