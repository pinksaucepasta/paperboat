package environment

import (
	"context"
	"errors"

	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/teams"
)

// CreateVaultTeam initializes encrypted ENV custody for an existing paid Team.
// Business Team creation and payment are owned by the Checkout producer.
func (s *Service) CreateVaultTeam(ctx context.Context, account string, request VaultTeamCreate) (VaultTeamState, error) {
	var out VaultTeamState
	if !validIdentifier(request.TeamID) {
		return out, ErrProtocolInvalid
	}
	scopeRaw, err := DecodeCanonicalBase64URL(request.ScopeEnvelope, MaximumVaultScopeBytes)
	if err != nil {
		return out, err
	}
	grantRaw, err := DecodeCanonicalBase64URL(request.GrantEnvelope, MaximumTeamGrantBytes)
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
		authority, err := teams.ReadTx(ctx, tx, request.TeamID)
		if errors.Is(err, teams.ErrNotFound) {
			return ErrKeyAuthorizationRequired
		}
		if err != nil {
			return err
		}
		if authority.Deleted || authority.Suspended || authority.OwnerAccount != account {
			return ErrKeyAuthorizationRequired
		}
		if err := requireVaultWorkspaceMemberTx(ctx, tx, account, request.TeamID); err != nil {
			return err
		}
		replay, digest, err := vaultOperationTx(ctx, tx, account, request.OperationID, "team-create", request, &out)
		if err != nil {
			return err
		}
		if replay {
			current, err := readVaultTeamTx(ctx, tx, request.TeamID)
			if err != nil {
				return err
			}
			if !authorizedVaultMember(current, account) {
				return ErrKeyAuthorizationRequired
			}
			if current.Generation != out.Generation {
				return ErrVersionConflict
			}
			return nil
		}
		if authority.Generation != request.ExpectedTeamGeneration {
			return ErrVersionConflict
		}
		var membership uint64
		for _, m := range authority.Members {
			if m.AccountID == account && m.Active {
				membership = m.MembershipGeneration
			}
		}
		if membership == 0 {
			return ErrKeyAuthorizationRequired
		}
		nextVault, writer, err := s.prepareVaultKeyMutationTx(ctx, tx, account, request.VaultEnvelope)
		if err != nil {
			return err
		}
		scope, err := parseVaultScope(scopeRaw, s.passwordVaultIssuer, writer)
		if err != nil {
			return err
		}
		grant, err := parseVaultGrant(grantRaw, s.passwordVaultIssuer, writer, nextVault.metadata)
		if err != nil {
			return err
		}
		if scope.Header.OwnerKind != "team" || scope.Header.OwnerID != request.TeamID || scope.Header.Revision != 1 || scope.Header.KeyEpoch != 1 || grant.Claims.TeamID != request.TeamID || grant.Claims.TeamEpoch != 1 || grant.Claims.MembershipGeneration != membership || grant.Claims.OperationID != request.OperationID {
			return ErrProtocolInvalid
		}
		var initialized bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM environment_vault_teams WHERE team_id=$1)`, request.TeamID).Scan(&initialized); err != nil {
			return err
		}
		if initialized {
			return ErrOperationConflict
		}
		if err := initializeVaultTeamAuthorityTx(ctx, tx, account, request.TeamID); err != nil {
			return err
		}
		if err := writeVaultScopeTx(ctx, tx, scope, writer.WriterPublicKey[:]); err != nil {
			return err
		}
		if err := writeVaultGrantTx(ctx, tx, grant); err != nil {
			return err
		}
		if err := commitVaultKeyMutationTx(ctx, tx, nextVault); err != nil {
			return err
		}
		out, err = readVaultTeamTx(ctx, tx, request.TeamID)
		if err != nil {
			return err
		}
		if err := teams.AuditTx(ctx, tx, account, request.TeamID, "env.initialize", request.OperationID, map[string]any{"generation": out.Generation, "epoch": out.KeyEpoch}); err != nil {
			return err
		}
		return saveVaultOperationTx(ctx, tx, account, request.OperationID, digest, out)
	})
	return out, err
}

// Caller holds the Team row and has checked current owner, membership and payment.
func initializeVaultTeamAuthorityTx(ctx context.Context, tx *db.Tx, account, team string) error {
	if _, err := tx.Exec(ctx, `UPDATE teams SET generation=generation+1 WHERE team_id=$1`, team); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO team_resource_bindings(team_id,resource_kind,resource_id,owner_account,active,generation) VALUES($1,'env',$1,$2,true,1)`, team, account); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO environment_vault_teams(team_id,key_epoch) VALUES($1,1)`, team); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO environment_vault_team_members(team_id,account_id,grant_epoch) VALUES($1,$2,1)`, team, account); err != nil {
		return err
	}
	return nil // the owner's ENV access is the team policy's owner guardrail
}

func commitVaultKeyMutationTx(ctx context.Context, tx *db.Tx, incoming parsedPasswordVault) error {
	_, err := tx.Exec(ctx, `UPDATE environment_password_vaults SET generation=$2,document_id=$3,envelope=$4,updated_at=now() WHERE account_id=$1`, incoming.metadata.Head.AccountID, incoming.metadata.Head.Generation, incoming.metadata.Head.DocumentID, incoming.raw)
	return err
}
