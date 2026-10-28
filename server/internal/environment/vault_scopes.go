package environment

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
)

// All vault scope writes take the team write lock before row locks. This small bounded subsystem avoids
// cross-account grant/reset deadlocks; password-only writes take an account lock and never wait for it.
func lockVaultScopes(ctx context.Context, tx *db.Tx) error {
	return db.LockTeamWrites(ctx, tx)
}
func (s *Service) vaultMetadataTx(ctx context.Context, tx *db.Tx, account string) (PasswordVaultMetadata, error) {
	// Match password publication lock order before reading custody or inserting user FKs.
	if err := lockAccountTx(ctx, tx, account); err != nil {
		return PasswordVaultMetadata{}, err
	}
	var raw []byte
	var generation uint64
	var id string
	err := tx.QueryRow(ctx, `SELECT generation,document_id,envelope FROM environment_password_vaults WHERE account_id=$1 FOR UPDATE`, account).Scan(&generation, &id, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return PasswordVaultMetadata{}, ErrNotFound
	}
	if err != nil {
		return PasswordVaultMetadata{}, err
	}
	metadata, err := ParsePasswordVaultMetadata(raw, s.passwordVaultIssuer, account)
	if err != nil || metadata.Head.Generation != generation || metadata.Head.DocumentID != id {
		return PasswordVaultMetadata{}, ErrProtocolInvalid
	}
	return metadata, nil
}
func vaultOperationTx(ctx context.Context, tx *db.Tx, account, op, kind string, request, result any) (bool, []byte, error) {
	if !validIdentifier(account) || !validIdentifier(op) {
		return false, nil, ErrProtocolInvalid
	}
	raw, err := json.Marshal([]any{kind, request})
	if err != nil {
		return false, nil, err
	}
	digest := sha256.Sum256(raw)
	var storedDigest, stored []byte
	err = tx.QueryRow(ctx, `SELECT request_digest,result FROM environment_vault_operations WHERE account_id=$1 AND operation_id=$2`, account, op).Scan(&storedDigest, &stored)
	if err == nil {
		if !bytes.Equal(storedDigest, digest[:]) {
			return false, nil, ErrOperationConflict
		}
		if json.Unmarshal(stored, result) != nil {
			return false, nil, ErrProtocolInvalid
		}
		return true, digest[:], nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, nil, err
	}
	return false, digest[:], nil
}
func saveVaultOperationTx(ctx context.Context, tx *db.Tx, account, op string, digest []byte, result any) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO environment_vault_operations(account_id,operation_id,request_digest,result) VALUES($1,$2,$3,$4)`, account, op, digest, raw)
	return err
}
func scopeWorkspace(kind, owner string) string {
	if kind == "team" {
		return owner
	}
	return "personal"
}
func readVaultScopeTx(ctx context.Context, tx *db.Tx, kind, owner, machine string) (VaultScopeState, error) {
	return readVaultScopeWorkspaceTx(ctx, tx, scopeWorkspace(kind, owner), kind, owner, machine)
}
func readVaultScopeWorkspaceTx(ctx context.Context, tx *db.Tx, workspace, kind, owner, machine string) (VaultScopeState, error) {
	out := VaultScopeState{WorkspaceID: workspace, OwnerKind: kind, OwnerID: owner, MachineID: machine}
	var raw, writer []byte
	err := tx.QueryRow(ctx, `SELECT key_epoch,revision,document_id,envelope,writer_public FROM environment_vault_scopes WHERE workspace_id=$1 AND owner_kind=$2 AND owner_id=$3 AND machine_id=$4`, workspace, kind, owner, machine).Scan(&out.KeyEpoch, &out.Revision, &out.DocumentID, &raw, &writer)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	out.Envelope = base64.RawURLEncoding.EncodeToString(raw)
	out.WriterPublic = base64.RawURLEncoding.EncodeToString(writer)
	return out, nil
}
func writeVaultScopeTx(ctx context.Context, tx *db.Tx, scope parsedVaultScope, writer []byte) error {
	h := scope.Header
	_, err := tx.Exec(ctx, `INSERT INTO environment_vault_scopes(workspace_id,owner_kind,owner_id,machine_id,key_epoch,revision,document_id,envelope,writer_public) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT(workspace_id,owner_kind,owner_id,machine_id) DO UPDATE SET key_epoch=EXCLUDED.key_epoch,revision=EXCLUDED.revision,document_id=EXCLUDED.document_id,envelope=EXCLUDED.envelope,writer_public=EXCLUDED.writer_public`, h.WorkspaceID, h.OwnerKind, h.OwnerID, h.MachineID, h.KeyEpoch, h.Revision, scope.State.DocumentID, scope.Raw, writer)
	if err != nil {
		return err
	}
	return invalidateVaultLayerSourceTx(ctx, tx, h.WorkspaceID, h.OwnerKind, h.OwnerID, h.MachineID)
}
func readVaultTeamTx(ctx context.Context, tx *db.Tx, team string) (VaultTeamState, error) {
	out := VaultTeamState{TeamID: team, Members: []VaultTeamMember{}}
	err := tx.QueryRow(ctx, `SELECT t.owner_account,t.generation,e.key_epoch,e.rotation_required,team_paid_entitled(t.team_id) FROM teams t JOIN environment_vault_teams e USING(team_id) WHERE t.team_id=$1 AND t.deleted_at IS NULL AND NOT t.suspended`, team).Scan(&out.OwnerAccount, &out.Generation, &out.KeyEpoch, &out.RotationRequired, &out.Entitled)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	rows, err := tx.Query(ctx, `SELECT m.account_id,m.membership_generation,CASE WHEN t.owner_account=m.account_id THEN 'owner' ELSE m.role END,m.active,COALESCE(e.grant_epoch,0),team_env_permission(m.account_id,m.team_id) FROM team_members m JOIN teams t USING(team_id) LEFT JOIN environment_vault_team_members e USING(team_id,account_id) WHERE m.team_id=$1 ORDER BY m.account_id`, team)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var m VaultTeamMember
		if err := rows.Scan(&m.AccountID, &m.MembershipGeneration, &m.Role, &m.Active, &m.GrantEpoch, &m.ENVPermission); err != nil {
			rows.Close()
			return out, err
		}
		out.Members = append(out.Members, m)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	out.Scope, err = readVaultScopeTx(ctx, tx, "team", team, "")
	if err != nil {
		return out, err
	}
	err = tx.QueryRow(ctx, `SELECT COALESCE((SELECT sequence FROM environment_vault_record_heads WHERE workspace_id=$1 AND owner_kind='team' AND owner_id=$1 AND machine_id='' AND key_epoch=$2),0)`, team, out.KeyEpoch).Scan(&out.RecordSequence)
	return out, err
}
func vaultMember(team VaultTeamState, account string) (VaultTeamMember, bool) {
	for _, m := range team.Members {
		if m.AccountID == account {
			return m, true
		}
	}
	return VaultTeamMember{}, false
}
func authorizedVaultMember(team VaultTeamState, account string) bool {
	m, ok := vaultMember(team, account)
	return ok && m.Active && m.GrantEpoch == team.KeyEpoch && (m.ENVPermission == "read" || m.ENVPermission == "write")
}
func authorizeVaultScopeTx(ctx context.Context, tx *db.Tx, account, kind, owner, machine string) (uint64, error) {
	if !validIdentifier(account) || !validIdentifier(owner) || (machine != "" && !validIdentifier(machine)) {
		return 0, ErrProtocolInvalid
	}
	switch kind {
	case "personal":
		if owner != account {
			return 0, ErrKeyAuthorizationRequired
		}
		if machine != "" {
			var id string
			if err := tx.QueryRow(ctx, `SELECT id FROM user_machines WHERE id=$1 AND user_id=$2 AND deleted_at IS NULL`, machine, account).Scan(&id); errors.Is(err, pgx.ErrNoRows) {
				return 0, ErrMachineNotFound
			} else if err != nil {
				return 0, err
			}
		}
		return personalVaultEpochTx(ctx, tx, account)
	case "team":
		if machine != "" {
			return 0, ErrInvalidScope
		}
		team, err := readVaultTeamTx(ctx, tx, owner)
		if err != nil {
			return 0, err
		}
		// Explain billing only after current membership, key and binding authority.
		// Other callers continue to receive the ordinary authorization denial.
		if authorizedVaultMember(team, account) && !team.Entitled {
			var authority bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users u JOIN team_resource_bindings b ON b.team_id=$2 AND b.resource_kind='env' AND b.resource_id=$2 AND b.active WHERE u.id=$1 AND u.status='active')`, account, owner).Scan(&authority); err != nil {
				return 0, err
			}
			if authority {
				return 0, ErrTeamEntitlementRequired
			}
		}
		if err := requireVaultWorkspaceMemberTx(ctx, tx, account, owner); err != nil {
			return 0, err
		}
		var bound bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM team_resource_bindings WHERE team_id=$1 AND resource_kind='env' AND resource_id=$1 AND active)`, owner).Scan(&bound); err != nil {
			return 0, err
		}
		if !bound {
			return 0, ErrKeyAuthorizationRequired
		}

		if !authorizedVaultMember(team, account) {
			return 0, ErrKeyAuthorizationRequired
		}
		return team.KeyEpoch, nil
	default:
		return 0, ErrInvalidScope
	}
}
func scopeSuccessor(current VaultScopeState, exists bool, incoming parsedVaultScope, epoch uint64) bool {
	if epoch != 0 && incoming.Header.KeyEpoch != epoch {
		return false
	}
	if !exists {
		return incoming.Header.Revision == 1 && allZeroBytes(incoming.Header.Previous)
	}
	return incoming.Header.Revision == current.Revision+1 && digestText(incoming.Header.Previous) == current.DocumentID && (epoch != 0 || incoming.Header.KeyEpoch == current.KeyEpoch)
}
func (s *Service) GetVaultScope(ctx context.Context, account, workspace, kind, owner, machine string) (VaultScopeState, error) {
	var out VaultScopeState
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := lockVaultScopes(ctx, tx); err != nil {
			return err
		}
		if _, err := authorizeVaultScopeWorkspaceTx(ctx, tx, account, workspace, kind, owner, machine); err != nil {
			return err
		}
		var err error
		out, err = readVaultScopeWorkspaceTx(ctx, tx, workspace, kind, owner, machine)
		return err
	})
	return out, err
}
func (s *Service) PutVaultScope(ctx context.Context, account, workspace, kind, owner, machine string, request VaultScopePut) (VaultScopeState, error) {
	var out VaultScopeState
	raw, err := DecodeCanonicalBase64URL(request.Envelope, MaximumVaultScopeBytes)
	if err != nil {
		return out, err
	}
	err = s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := lockVaultScopes(ctx, tx); err != nil {
			return err
		}
		if err := allowVaultWritesTx(ctx, tx, account); err != nil {
			return err
		}
		if kind == "team" {
			if err := allowVaultTeamWritesTx(ctx, tx, owner); err != nil {
				return err
			}
		}
		if kind == "team" {
			team, err := readVaultTeamTx(ctx, tx, owner)
			if err != nil {
				return err
			}
			member, ok := vaultMember(team, account)
			if !ok || !member.Active || member.ENVPermission != "write" {
				return ErrKeyAuthorizationRequired
			}
		}
		epoch, err := authorizeVaultScopeWorkspaceTx(ctx, tx, account, workspace, kind, owner, machine)
		if err != nil {
			return err
		}
		replay, digest, err := vaultOperationTx(ctx, tx, account, request.OperationID, "scope:"+kind+":"+owner+":"+machine, request, &out)
		if err != nil || replay {
			return err
		}
		writer, err := s.vaultMetadataTx(ctx, tx, account)
		if err != nil {
			return err
		}
		scope, err := parseVaultScope(raw, s.passwordVaultIssuer, writer)
		if err != nil {
			return err
		}
		if scope.Header.WorkspaceID != workspace || scope.Header.OwnerKind != kind || scope.Header.OwnerID != owner || scope.Header.MachineID != machine {
			return ErrProtocolInvalid
		}
		current, err := readVaultScopeWorkspaceTx(ctx, tx, workspace, kind, owner, machine)
		exists := err == nil
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if kind == "personal" && !exists {
			var count int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM environment_vault_scopes WHERE owner_kind='personal' AND owner_id=$1`, account).Scan(&count); err != nil {
				return err
			}
			if count >= 513 {
				return ErrLimitExceeded
			}
		}
		if !scopeSuccessor(current, exists, scope, epoch) {
			return ErrVersionConflict
		}
		if err := writeVaultScopeTx(ctx, tx, scope, writer.WriterPublicKey[:]); err != nil {
			return err
		}
		out = scope.State
		if kind == "team" {
			if err := teams.AuditTx(ctx, tx, account, owner, "env.write", request.OperationID, map[string]any{"revision": out.Revision, "epoch": out.KeyEpoch}); err != nil {
				return err
			}
		}
		return saveVaultOperationTx(ctx, tx, account, request.OperationID, digest, out)
	})
	return out, err
}
func (s *Service) GetVaultSharing(ctx context.Context, account, target string) (VaultSharingState, error) {
	var out VaultSharingState
	if !validIdentifier(account) || !validIdentifier(target) {
		return out, ErrProtocolInvalid
	}
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		m, err := s.vaultMetadataTx(ctx, tx, target)
		if err != nil {
			return err
		}
		out = VaultSharingState{AccountID: target, VaultGeneration: m.Head.Generation, WriterPublic: base64.RawURLEncoding.EncodeToString(m.WriterPublicKey[:]), SharingPublic: base64.RawURLEncoding.EncodeToString(m.SharingPublicKey[:])}
		return nil
	})
	return out, err
}
func (s *Service) GetVaultTeam(ctx context.Context, account, team string) (VaultTeamState, error) {
	var out VaultTeamState
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := lockVaultScopes(ctx, tx); err != nil {
			return err
		}
		var err error
		out, err = readVaultTeamTx(ctx, tx, team)
		if err != nil {
			return err
		}
		m, ok := vaultMember(out, account)
		if !ok || !m.Active {
			return ErrKeyAuthorizationRequired
		}
		// Total-loss recovery needs the signed encrypted predecessor, even after
		// a Personal reset fences the owner's previous key grant. This grants no
		// scope read/write authority and exposes no key or plaintext.
		if !authorizedVaultMember(out, account) && !vaultTeamRotationAllowed(out, account, VaultTeamRotate{ConfirmTotalLoss: true}) {
			out.Scope.Envelope = ""
			out.Scope.WriterPublic = ""
		}
		return nil
	})
	return out, err
}
func writeVaultGrantTx(ctx context.Context, tx *db.Tx, grant parsedVaultGrant) error {
	c := grant.Claims
	_, err := tx.Exec(ctx, `INSERT INTO environment_vault_team_grants(team_id,account_id,membership_generation,team_epoch,recipient_vault_generation,recipient_sharing_public,document_id,envelope,sender_writer_public,acknowledged) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,false) ON CONFLICT(team_id,account_id) DO UPDATE SET membership_generation=EXCLUDED.membership_generation,team_epoch=EXCLUDED.team_epoch,recipient_vault_generation=EXCLUDED.recipient_vault_generation,recipient_sharing_public=EXCLUDED.recipient_sharing_public,document_id=EXCLUDED.document_id,envelope=EXCLUDED.envelope,sender_writer_public=EXCLUDED.sender_writer_public,acknowledged=false`, c.TeamID, c.RecipientAccount, c.MembershipGeneration, c.TeamEpoch, c.RecipientVaultGeneration, c.RecipientSharingPublic, grant.State.DocumentID, grant.Raw, c.SenderWriterPublic)
	return err
}

// prepareVaultKeyMutationTx validates the owner's exact staged key inventory.
func (s *Service) prepareVaultKeyMutationTx(ctx context.Context, tx *db.Tx, account, encoded string) (parsedPasswordVault, PasswordVaultMetadata, error) {
	raw, err := DecodeCanonicalBase64URL(encoded, MaximumPasswordVaultBytes)
	if err != nil {
		return parsedPasswordVault{}, PasswordVaultMetadata{}, err
	}
	incoming, err := parsePasswordVault(raw, s.passwordVaultIssuer, account)
	if err != nil {
		return incoming, PasswordVaultMetadata{}, err
	}
	current, err := s.vaultMetadataTx(ctx, tx, account)
	if err != nil {
		return incoming, current, err
	}
	currentRaw, err := DecodeCanonicalBase64URL(current.Head.Envelope, MaximumPasswordVaultBytes)
	if err != nil {
		return incoming, current, err
	}
	previous, err := parsePasswordVault(currentRaw, s.passwordVaultIssuer, account)
	if err != nil {
		return incoming, current, err
	}
	if incoming.metadata.Head.Generation != current.Head.Generation+1 || digestText(incoming.metadata.PreviousDigest[:]) != current.Head.DocumentID || !validPasswordVaultSuccessor(previous, incoming) || incoming.metadata.SharingPublicKey != current.SharingPublicKey || !bytes.Equal(previous.passwordDescriptor, incoming.passwordDescriptor) || !bytes.Equal(previous.recoveryDescriptor, incoming.recoveryDescriptor) {
		return incoming, current, ErrPasswordVaultConflict
	}
	return incoming, current, nil
}
func (s *Service) GrantVaultTeamMember(ctx context.Context, account, teamID string, request VaultMemberGrant) (VaultTeamState, error) {
	var out VaultTeamState
	raw, err := DecodeCanonicalBase64URL(request.GrantEnvelope, MaximumTeamGrantBytes)
	if err != nil {
		return out, err
	}
	if !validIdentifier(request.AccountID) {
		return out, ErrProtocolInvalid
	}
	err = s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := lockVaultScopes(ctx, tx); err != nil {
			return err
		}
		if err := allowVaultWritesTx(ctx, tx, account); err != nil {
			return err
		}
		team, err := readVaultTeamTx(ctx, tx, teamID)
		if err != nil {
			return err
		}
		actor, actorExists := vaultMember(team, account)
		if !actorExists || (actor.Role != "owner" && actor.Role != "admin") || !authorizedVaultMember(team, account) {
			return ErrKeyAuthorizationRequired
		}
		replay, digest, err := vaultOperationTx(ctx, tx, account, request.OperationID, "team-grant:"+teamID, request, &out)
		if err != nil {
			return err
		}
		if replay {
			member, ok := vaultMember(team, request.AccountID)
			if !ok || !member.Active || (member.ENVPermission != "read" && member.ENVPermission != "write") {
				return ErrKeyAuthorizationRequired
			}
			if team.Generation != out.Generation || member.MembershipGeneration != request.ExpectedMembershipGeneration {
				return ErrVersionConflict
			}
			return nil
		}
		if team.Generation != request.ExpectedTeamGeneration {
			return ErrVersionConflict
		}
		member, exists := vaultMember(team, request.AccountID)
		if (!exists && request.ExpectedMembershipGeneration != 0) || (exists && member.MembershipGeneration != request.ExpectedMembershipGeneration) {
			return ErrVersionConflict
		}
		if !exists || !member.Active || (member.ENVPermission != "read" && member.ENVPermission != "write") {
			return ErrKeyAuthorizationRequired
		}
		if team.RotationRequired {
			return ErrVaultRotationRequired
		}
		generation := member.MembershipGeneration
		sender, err := s.vaultMetadataTx(ctx, tx, account)
		if err != nil {
			return err
		}
		recipient := sender
		if request.AccountID != account {
			recipient, err = s.vaultMetadataTx(ctx, tx, request.AccountID)
			if err != nil {
				return err
			}
		}
		grant, err := parseVaultGrant(raw, s.passwordVaultIssuer, sender, recipient)
		if err != nil {
			return err
		}
		if grant.Claims.TeamID != teamID || grant.Claims.TeamEpoch != team.KeyEpoch || grant.Claims.MembershipGeneration != generation || grant.Claims.OperationID != request.OperationID {
			return ErrProtocolInvalid
		}
		if _, err := tx.Exec(ctx, `INSERT INTO environment_vault_team_members(team_id,account_id,grant_epoch) VALUES($1,$2,$3) ON CONFLICT(team_id,account_id) DO UPDATE SET grant_epoch=EXCLUDED.grant_epoch`, teamID, request.AccountID, team.KeyEpoch); err != nil {
			return err
		}
		if err := writeVaultGrantTx(ctx, tx, grant); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE teams SET generation=generation+1 WHERE team_id=$1`, teamID); err != nil {
			return err
		}
		out, err = readVaultTeamTx(ctx, tx, teamID)
		if err != nil {
			return err
		}
		if err := teams.AuditTx(ctx, tx, account, teamID, "env.key_grant", request.OperationID, map[string]any{"generation": out.Generation, "epoch": out.KeyEpoch}); err != nil {
			return err
		}
		return saveVaultOperationTx(ctx, tx, account, request.OperationID, digest, out)
	})
	return out, err
}

func (s *Service) RotateVaultTeam(ctx context.Context, account, teamID string, request VaultTeamRotate) (VaultTeamState, error) {
	var out VaultTeamState
	if request.RemoveAccountIDs == nil || request.GrantEnvelopes == nil || len(request.RemoveAccountIDs) > MaximumVaultTeamMembers || len(request.GrantEnvelopes) > MaximumVaultTeamMembers {
		return out, ErrProtocolInvalid
	}
	scopeRaw, err := DecodeCanonicalBase64URL(request.ScopeEnvelope, MaximumVaultScopeBytes)
	if err != nil {
		return out, err
	}
	err = s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := lockVaultScopes(ctx, tx); err != nil {
			return err
		}
		team, err := readVaultTeamTx(ctx, tx, teamID)
		if err != nil {
			return err
		}
		actorRequest := request
		actorRequest.RemoveAccountIDs = nil
		if !vaultTeamRotationAllowed(team, account, actorRequest) {
			return ErrKeyAuthorizationRequired
		}
		replay, digest, err := vaultOperationTx(ctx, tx, account, request.OperationID, "team-rotate:"+teamID, request, &out)
		if err != nil {
			return err
		}
		if replay {
			if out.Generation != team.Generation {
				return ErrVersionConflict
			}
			actor, _ := vaultMember(team, account)
			for _, id := range request.RemoveAccountIDs {
				target, ok := vaultMember(team, id)
				if !ok || id == team.OwnerAccount || (actor.Role == "admin" && (target.Role == "owner" || target.Role == "admin")) {
					return ErrKeyAuthorizationRequired
				}
			}
			return nil
		}
		if !vaultTeamRotationAllowed(team, account, request) {
			return ErrKeyAuthorizationRequired
		}
		if team.Generation != request.ExpectedTeamGeneration || team.KeyEpoch >= MaxBrowserInteger {
			return ErrVersionConflict
		}
		removed := map[string]bool{}
		for _, id := range request.RemoveAccountIDs {
			m, ok := vaultMember(team, id)
			if !ok || (!m.Active && !team.RotationRequired) || id == account || removed[id] {
				return ErrProtocolInvalid
			}
			removed[id] = true
		}
		nextVault, sender, err := s.prepareVaultKeyMutationTx(ctx, tx, account, request.VaultEnvelope)
		if err != nil {
			return err
		}
		scope, err := parseVaultScope(scopeRaw, s.passwordVaultIssuer, sender)
		if err != nil {
			return err
		}
		if scope.Header.OwnerKind != "team" || scope.Header.OwnerID != teamID || !scopeSuccessor(team.Scope, true, scope, team.KeyEpoch+1) {
			return ErrVersionConflict
		}
		remaining := map[string]VaultTeamMember{}
		for _, m := range team.Members {
			if m.Active && (m.ENVPermission == "read" || m.ENVPermission == "write") && !removed[m.AccountID] {
				remaining[m.AccountID] = m
			}
		}
		if len(request.GrantEnvelopes) != len(remaining) {
			return ErrPrecondition
		}
		grants := make([]parsedVaultGrant, 0, len(remaining))
		seen := map[string]bool{}
		for _, encoded := range request.GrantEnvelopes {
			raw, err := DecodeCanonicalBase64URL(encoded, MaximumTeamGrantBytes)
			if err != nil {
				return err
			}
			var env vaultTeamGrantEnvelope
			var c vaultTeamGrantClaims
			if !decodeVaultCanonical(raw, MaximumTeamGrantBytes, &env) || !decodeVaultCanonical(env.Claims, 2048, &c) {
				return ErrProtocolInvalid
			}
			m, ok := remaining[c.RecipientAccount]
			if !ok || seen[c.RecipientAccount] {
				return ErrPrecondition
			}
			seen[c.RecipientAccount] = true
			recipient := nextVault.metadata
			if c.RecipientAccount != account {
				recipient, err = s.vaultMetadataTx(ctx, tx, c.RecipientAccount)
				if err != nil {
					return err
				}
			}
			grant, err := parseVaultGrant(raw, s.passwordVaultIssuer, sender, recipient)
			if err != nil {
				return err
			}
			if c.TeamID != teamID || c.TeamEpoch != team.KeyEpoch+1 || c.MembershipGeneration != m.MembershipGeneration || c.OperationID != request.OperationID {
				return ErrProtocolInvalid
			}
			grants = append(grants, grant)
		}
		for id := range removed {
			member, _ := vaultMember(team, id)
			if !member.Active {
				// Unified membership removal already revoked grants and fenced ENV.
				// Finish the pending rekey without repeating that transition.
				continue
			}
			if err := teams.RemoveMemberTx(ctx, tx, teamID, id); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM environment_vault_team_grants WHERE team_id=$1`, teamID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE environment_vault_teams SET key_epoch=key_epoch+1,rotation_required=false WHERE team_id=$1`, teamID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE teams SET generation=generation+1 WHERE team_id=$1`, teamID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE environment_vault_team_members SET grant_epoch=0 WHERE team_id=$1`, teamID); err != nil {
			return err
		}
		for id := range remaining {
			if _, err := tx.Exec(ctx, `INSERT INTO environment_vault_team_members(team_id,account_id,grant_epoch) VALUES($1,$2,$3) ON CONFLICT(team_id,account_id) DO UPDATE SET grant_epoch=EXCLUDED.grant_epoch`, teamID, id, team.KeyEpoch+1); err != nil {
				return err
			}
		}
		for id := range removed {
			if _, err := tx.Exec(ctx, `UPDATE team_resource_grants SET active=false,generation=generation+1 WHERE team_id=$1 AND account_id=$2 AND resource_kind<>'env' AND active`, teamID, id); err != nil {
				return err
			}
			// The team ENV grant is the member's rule in the team policy (the vault lock is the teams lock).
			if err := teams.ApplyENVAccessTx(ctx, tx, config.WorkspacePolicy{}, teamID, account, id, ""); err != nil {
				return err
			}
		}
		if err := writeVaultScopeTx(ctx, tx, scope, sender.WriterPublicKey[:]); err != nil {
			return err
		}
		if _, err := s.RebuildVaultRecordsTx(ctx, tx, VaultLayerCoordinate{WorkspaceID: teamID, OwnerKind: "team", OwnerID: teamID}, request.Records.ExpectedSequence, request.Records.DocumentIDs, scope.Header.KeyEpoch, sender, request.Records.Envelopes, request.ConfirmTotalLoss); err != nil {
			return err
		}
		for _, grant := range grants {
			if err := writeVaultGrantTx(ctx, tx, grant); err != nil {
				return err
			}
		}
		if err := commitVaultKeyMutationTx(ctx, tx, nextVault); err != nil {
			return err
		}
		out, err = readVaultTeamTx(ctx, tx, teamID)
		if err != nil {
			return err
		}
		action := "env.rotate"
		if request.ConfirmTotalLoss {
			action = "env.reset"
		}
		if err := teams.AuditTx(ctx, tx, account, teamID, action, request.OperationID, map[string]any{"generation": out.Generation, "epoch": out.KeyEpoch}); err != nil {
			return err
		}
		return saveVaultOperationTx(ctx, tx, account, request.OperationID, digest, out)
	})
	return out, err
}

func (s *Service) ListVaultGrants(ctx context.Context, account string) ([]VaultGrantState, error) {
	out := []VaultGrantState{}
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		current, err := s.vaultMetadataTx(ctx, tx, account)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT g.team_id,g.membership_generation,g.team_epoch,g.document_id,g.envelope,g.sender_writer_public,g.acknowledged FROM environment_vault_team_grants g JOIN team_members m USING(team_id,account_id) JOIN environment_vault_team_members e USING(team_id,account_id) JOIN environment_vault_teams t USING(team_id) JOIN teams authority USING(team_id) JOIN users recipient ON recipient.id=g.account_id AND recipient.status='active' JOIN team_resource_bindings binding ON binding.team_id=g.team_id AND binding.resource_kind='env' AND binding.resource_id=g.team_id AND binding.active WHERE g.account_id=$1 AND m.active AND authority.deleted_at IS NULL AND NOT authority.suspended AND NOT t.rotation_required AND team_paid_entitled(g.team_id) AND e.grant_epoch=t.key_epoch AND g.team_epoch=t.key_epoch AND g.membership_generation=m.membership_generation AND g.recipient_sharing_public=$2 ORDER BY g.team_id LIMIT 129`, account, current.SharingPublicKey[:])
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var grant VaultGrantState
			var raw, writer []byte
			if err := rows.Scan(&grant.TeamID, &grant.MembershipGeneration, &grant.TeamEpoch, &grant.DocumentID, &raw, &writer, &grant.Acknowledged); err != nil {
				return err
			}
			grant.Envelope = base64.RawURLEncoding.EncodeToString(raw)
			grant.SenderWriterPublic = base64.RawURLEncoding.EncodeToString(writer)
			out = append(out, grant)
		}
		if len(out) > MaximumVaultTeamMembers {
			return ErrLimitExceeded
		}
		return rows.Err()
	})
	return out, err
}
func (s *Service) AcknowledgeVaultGrant(ctx context.Context, account, documentID, vaultID string) error {
	if !digestExpression.MatchString(documentID) || !digestExpression.MatchString(vaultID) {
		return ErrProtocolInvalid
	}
	return s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := lockVaultScopes(ctx, tx); err != nil {
			return err
		}
		current, err := s.vaultMetadataTx(ctx, tx, account)
		if err != nil {
			return err
		}
		if current.Head.DocumentID != vaultID {
			return ErrPasswordVaultConflict
		}
		command, err := tx.Exec(ctx, `UPDATE environment_vault_team_grants g SET acknowledged=true FROM team_members m,environment_vault_team_members e,environment_vault_teams t,teams authority,team_resource_bindings binding,users recipient WHERE g.account_id=$1 AND g.document_id=$2 AND g.recipient_sharing_public=$3 AND m.account_id=g.account_id AND m.team_id=g.team_id AND m.active AND m.membership_generation=g.membership_generation AND e.team_id=g.team_id AND e.account_id=g.account_id AND e.grant_epoch=g.team_epoch AND t.team_id=g.team_id AND t.key_epoch=g.team_epoch AND NOT t.rotation_required AND authority.team_id=t.team_id AND authority.deleted_at IS NULL AND NOT authority.suspended AND team_paid_entitled(g.team_id) AND binding.team_id=t.team_id AND binding.resource_kind='env' AND binding.resource_id=t.team_id AND binding.active AND recipient.id=g.account_id AND recipient.status='active'`, account, documentID, current.SharingPublicKey[:])
		if err != nil {
			return err
		}
		if command.RowsAffected() != 1 {
			return ErrNotFound
		}
		return nil
	})
}

func (s *Service) ResetPersonalVault(ctx context.Context, account string, request VaultPersonalReset) (PasswordVaultHead, error) {
	return s.mutatePersonalVault(ctx, account, request, true, nil)
}
func (s *Service) RotatePersonalVault(ctx context.Context, account string, request VaultPersonalRotate) (PasswordVaultHead, error) {
	return s.mutatePersonalVault(ctx, account, VaultPersonalReset{OperationID: request.OperationID, ExpectedVaultDocumentID: request.ExpectedVaultDocumentID, VaultEnvelope: request.VaultEnvelope, ScopeEnvelopes: []string{}}, false, request.ScopeDocuments)
}
func (s *Service) mutatePersonalVault(ctx context.Context, account string, request VaultPersonalReset, reset bool, documents []VaultScopeDocument) (PasswordVaultHead, error) {
	var out PasswordVaultHead
	if (reset && !request.ConfirmTotalLoss) || !digestExpression.MatchString(request.ExpectedVaultDocumentID) || (reset && (request.ScopeEnvelopes == nil || len(request.ScopeEnvelopes) > 513)) || (!reset && (documents == nil || len(documents) > 513)) {
		return out, ErrPrecondition
	}
	raw, err := DecodeCanonicalBase64URL(request.VaultEnvelope, MaximumPasswordVaultBytes)
	if err != nil {
		return out, err
	}
	incoming, err := parsePasswordVault(raw, s.passwordVaultIssuer, account)
	if err != nil {
		return out, err
	}
	err = s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := lockVaultScopes(ctx, tx); err != nil {
			return err
		}
		if err := lockAccountTx(ctx, tx, account); err != nil {
			return err
		}
		replay, digest, err := vaultOperationTx(ctx, tx, account, request.OperationID, map[bool]string{true: "personal-reset", false: "personal-rotate"}[reset], []any{request, documents}, &out)
		if err != nil || replay {
			return err
		}
		current, err := s.vaultMetadataTx(ctx, tx, account)
		if err != nil {
			return err
		}
		if current.Head.DocumentID != request.ExpectedVaultDocumentID || incoming.metadata.Head.Generation != current.Head.Generation+1 || digestText(incoming.metadata.PreviousDigest[:]) != current.Head.DocumentID {
			return ErrPasswordVaultConflict
		}
		if reset {
			if incoming.metadata.PasswordEpoch != current.PasswordEpoch+1 || incoming.metadata.RecoveryEpoch != current.RecoveryEpoch+1 || incoming.metadata.WriterPublicKey == current.WriterPublicKey || incoming.metadata.SharingPublicKey == current.SharingPublicKey {
				return ErrPasswordVaultConflict
			}
		} else {
			previousRaw, err := DecodeCanonicalBase64URL(current.Head.Envelope, MaximumPasswordVaultBytes)
			if err != nil {
				return err
			}
			previous, err := parsePasswordVault(previousRaw, s.passwordVaultIssuer, account)
			if err != nil {
				return err
			}
			if !validPasswordVaultSuccessor(previous, incoming) || incoming.metadata.SharingPublicKey != current.SharingPublicKey || !bytes.Equal(previous.passwordDescriptor, incoming.passwordDescriptor) || !bytes.Equal(previous.recoveryDescriptor, incoming.recoveryDescriptor) {
				return ErrPasswordVaultConflict
			}
		}
		personalEpoch, err := personalVaultEpochTx(ctx, tx, account)
		if err != nil {
			return err
		}
		if personalEpoch >= MaxBrowserInteger {
			return ErrVersionConflict
		}
		rows, err := tx.Query(ctx, `SELECT workspace_id,machine_id FROM environment_vault_scopes scope WHERE owner_kind='personal' AND owner_id=$1 AND (machine_id='' OR EXISTS (SELECT 1 FROM user_machines m WHERE m.id=scope.machine_id AND m.deleted_at IS NULL)) ORDER BY workspace_id,machine_id`, account)
		if err != nil {
			return err
		}
		coordinates := []VaultScopeDocument{}
		for rows.Next() {
			var coordinate VaultScopeDocument
			if err := rows.Scan(&coordinate.WorkspaceID, &coordinate.MachineID); err != nil {
				rows.Close()
				return err
			}
			coordinates = append(coordinates, coordinate)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if (reset && len(request.ScopeEnvelopes) != len(coordinates)) || (!reset && len(documents) != len(coordinates)) {
			return ErrPrecondition
		}
		sort.Slice(coordinates, func(i, j int) bool {
			return scopeDocumentKey(coordinates[i].WorkspaceID, coordinates[i].MachineID) < scopeDocumentKey(coordinates[j].WorkspaceID, coordinates[j].MachineID)
		})
		scopes := map[string]parsedVaultScope{}
		for _, encoded := range request.ScopeEnvelopes {
			raw, err := DecodeCanonicalBase64URL(encoded, MaximumVaultScopeBytes)
			if err != nil {
				return err
			}
			scope, err := parseVaultScope(raw, s.passwordVaultIssuer, incoming.metadata)
			if err != nil {
				return err
			}
			if scope.Header.OwnerKind != "personal" || scope.Header.OwnerID != account {
				return ErrProtocolInvalid
			}
			if _, ok := scopes[scopeDocumentKey(scope.Header.WorkspaceID, scope.Header.MachineID)]; ok {
				return ErrProtocolInvalid
			}
			scopes[scopeDocumentKey(scope.Header.WorkspaceID, scope.Header.MachineID)] = scope
		}

		staged := map[string]string{}
		for _, document := range documents {
			if !validIdentifier(document.WorkspaceID) || !digestExpression.MatchString(document.DocumentID) || (document.MachineID != "" && !validIdentifier(document.MachineID)) {
				return ErrProtocolInvalid
			}
			if _, ok := staged[scopeDocumentKey(document.WorkspaceID, document.MachineID)]; ok {
				return ErrProtocolInvalid
			}
			staged[scopeDocumentKey(document.WorkspaceID, document.MachineID)] = document.DocumentID
		}
		for _, coordinate := range coordinates {
			workspaceID, machine := coordinate.WorkspaceID, coordinate.MachineID
			key := scopeDocumentKey(workspaceID, machine)
			var replacement VaultRecordReplacement
			if reset {
				if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT sequence FROM environment_vault_record_heads WHERE workspace_id=$1 AND owner_kind='personal' AND owner_id=$2 AND machine_id=$3),0)`, workspaceID, account, machine).Scan(&replacement.ExpectedSequence); err != nil {
					return err
				}
			}
			if !reset {
				expected, ok := staged[key]
				if !ok {
					return ErrPrecondition
				}
				var stagedRaw []byte
				var stagedID string
				var stagedRecords []byte
				err := tx.QueryRow(ctx, `SELECT s.document_id,s.envelope,s.record_replacement FROM environment_vault_personal_rotation_scopes s JOIN environment_vault_personal_rotations r USING(account_id,operation_id) WHERE s.account_id=$1 AND s.operation_id=$2 AND s.workspace_id=$3 AND s.machine_id=$4 AND r.expected_vault_document_id=$5`, account, request.OperationID, workspaceID, machine, request.ExpectedVaultDocumentID).Scan(&stagedID, &stagedRaw, &stagedRecords)
				if errors.Is(err, pgx.ErrNoRows) {
					return ErrPrecondition
				}
				if err != nil {
					return err
				}
				if stagedID != expected {
					return ErrOperationConflict
				}
				if err := json.Unmarshal(stagedRecords, &replacement); err != nil {
					return err
				}
				parsed, err := parseVaultScope(stagedRaw, s.passwordVaultIssuer, incoming.metadata)
				if err != nil {
					return err
				}
				if parsed.Header.WorkspaceID != workspaceID || parsed.Header.OwnerKind != "personal" || parsed.Header.OwnerID != account || parsed.Header.MachineID != machine || parsed.State.DocumentID != expected {
					return ErrProtocolInvalid
				}
				scopes[key] = parsed
			}
			scope, ok := scopes[key]
			if !ok {
				return ErrPrecondition
			}
			old, err := readVaultScopeWorkspaceTx(ctx, tx, workspaceID, "personal", account, machine)
			if err != nil {
				return err
			}
			if !scopeSuccessor(old, true, scope, personalEpoch+1) {
				return ErrVersionConflict
			}
			if err := writeVaultScopeTx(ctx, tx, scope, incoming.metadata.WriterPublicKey[:]); err != nil {
				return err
			}
			if _, err := s.RebuildVaultRecordsTx(ctx, tx, VaultLayerCoordinate{WorkspaceID: workspaceID, OwnerKind: "personal", OwnerID: account, MachineID: machine}, replacement.ExpectedSequence, replacement.DocumentIDs, scope.Header.KeyEpoch, incoming.metadata, replacement.Envelopes, reset); err != nil {
				return err
			}
			delete(scopes, key)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO environment_vault_personal_epochs(account_id,key_epoch,rotation_required) VALUES($1,$2,false) ON CONFLICT(account_id) DO UPDATE SET key_epoch=EXCLUDED.key_epoch,rotation_required=false`, account, personalEpoch+1); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE environment_password_vaults SET generation=$2,document_id=$3,envelope=$4,updated_at=now() WHERE account_id=$1`, account, incoming.metadata.Head.Generation, incoming.metadata.Head.DocumentID, raw); err != nil {
			return err
		}
		if reset {
			if _, err := tx.Exec(ctx, `UPDATE teams SET generation=generation+1 WHERE team_id IN(SELECT team_id FROM team_members WHERE account_id=$1 AND active)`, account); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE environment_vault_team_members SET grant_epoch=0 WHERE account_id=$1`, account); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM environment_vault_team_grants WHERE account_id=$1`, account); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM environment_vault_personal_rotations WHERE account_id=$1`, account); err != nil {
			return err
		}
		out = incoming.metadata.Head
		return saveVaultOperationTx(ctx, tx, account, request.OperationID, digest, out)
	})
	return out, err
}

func personalVaultEpochTx(ctx context.Context, tx *db.Tx, account string) (uint64, error) {
	var epoch uint64
	err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT key_epoch FROM environment_vault_personal_epochs WHERE account_id=$1),1)`, account).Scan(&epoch)
	return epoch, err
}
func (s *Service) GetVaultPersonalScopes(ctx context.Context, account string) (VaultPersonalScopes, error) {
	out := VaultPersonalScopes{Scopes: []VaultScopeState{}}
	if !validIdentifier(account) {
		return out, ErrProtocolInvalid
	}
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := lockVaultScopes(ctx, tx); err != nil {
			return err
		}
		var err error
		out.KeyEpoch, err = personalVaultEpochTx(ctx, tx, account)
		if err == nil {
			out.RotationRequired, err = personalVaultRotationRequiredTx(ctx, tx, account)
		}
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT workspace_id,machine_id,key_epoch,revision,document_id,writer_public FROM environment_vault_scopes scope WHERE owner_kind='personal' AND owner_id=$1 AND (machine_id='' OR EXISTS (SELECT 1 FROM user_machines m WHERE m.id=scope.machine_id AND m.deleted_at IS NULL)) ORDER BY workspace_id,machine_id LIMIT 514`, account)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			state := VaultScopeState{OwnerKind: "personal", OwnerID: account}
			var writer []byte
			if err := rows.Scan(&state.WorkspaceID, &state.MachineID, &state.KeyEpoch, &state.Revision, &state.DocumentID, &writer); err != nil {
				return err
			}
			state.WriterPublic = base64.RawURLEncoding.EncodeToString(writer)
			out.Scopes = append(out.Scopes, state)
		}
		if len(out.Scopes) > 513 {
			return ErrLimitExceeded
		}
		return rows.Err()
	})
	return out, err
}

var ErrVaultRotationRequired = errors.New("ENV key rotation is required")

// LockVaultMutationsTx coordinates explicit machine revocation with ENV publication.
// Call before acquiring session/machine locks in the owning revocation transaction.
func LockVaultMutationsTx(ctx context.Context, tx *db.Tx) error { return lockVaultScopes(ctx, tx) }
func personalVaultRotationRequiredTx(ctx context.Context, tx *db.Tx, account string) (bool, error) {
	var required bool
	err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT rotation_required FROM environment_vault_personal_epochs WHERE account_id=$1),false)`, account).Scan(&required)
	return required, err
}
func allowVaultWritesTx(ctx context.Context, tx *db.Tx, account string) error {
	required, err := personalVaultRotationRequiredTx(ctx, tx, account)
	if err != nil {
		return err
	}
	if required {
		return ErrVaultRotationRequired
	}
	return nil
}

func allowVaultTeamWritesTx(ctx context.Context, tx *db.Tx, team string) error {
	var required bool
	err := tx.QueryRow(ctx, `SELECT rotation_required FROM environment_vault_teams WHERE team_id=$1`, team).Scan(&required)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if required {
		return ErrVaultRotationRequired
	}
	return nil
}

func vaultTeamRotationAllowed(team VaultTeamState, account string, request VaultTeamRotate) bool {
	member, exists := vaultMember(team, account)
	if !exists || !member.Active || (member.Role != "owner" && member.Role != "admin") {
		return false
	}
	if request.ConfirmTotalLoss && (member.Role != "owner" || team.OwnerAccount != account) {
		return false
	}
	if !request.ConfirmTotalLoss && !authorizedVaultMember(team, account) {
		return false
	}
	for _, id := range request.RemoveAccountIDs {
		target, ok := vaultMember(team, id)
		if !ok || (!target.Active && !team.RotationRequired) || id == team.OwnerAccount || id == account {
			return false
		}
		if member.Role == "admin" && (target.Role == "owner" || target.Role == "admin") {
			return false
		}
	}
	return true
}
