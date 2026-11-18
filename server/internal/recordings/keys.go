package recordings

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/environment"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
	"github.com/pinksaucepasta/paperboat/shared/recording"
)

// memberTx returns account's role in team, refusing non-members, deleted or suspended teams, and teams without the
// paid entitlement when paid is required.
func memberTx(ctx context.Context, tx *db.Tx, account, team string, permission teams.Permission) (string, error) {
	var role *string
	var paid bool
	err := tx.QueryRow(ctx, `SELECT (SELECT CASE WHEN t.owner_account = $2 THEN 'owner' ELSE m.role END FROM team_members m WHERE m.team_id = t.team_id AND m.account_id = $2 AND m.active), team_paid_entitled(t.team_id)
 FROM teams t WHERE t.team_id = $1 AND t.deleted_at IS NULL AND NOT t.suspended`, team, account).Scan(&role, &paid)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && role == nil {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if !teams.Allowed(*role, permission) {
		return "", ErrForbidden
	}
	if !paid {
		return "", ErrNeedsTeam
	}
	return *role, teams.SSOGuardTx(ctx, tx, team, account)
}

func (s *Service) sharingKeyTx(ctx context.Context, tx *db.Tx, account string) ([]byte, error) {
	keys, err := s.sharingKeys(ctx, tx, []string{account})
	return keys[account], err
}

// vaultSharingKeys is each account's current vault sharing public key (what a sealed copy must be sealed to) in one
// query; accounts without a vault are left out.
func (s *Service) vaultSharingKeys(ctx context.Context, tx *db.Tx, accounts []string) (map[string][]byte, error) {
	rows, err := tx.Query(ctx, `SELECT account_id, envelope FROM environment_password_vaults WHERE account_id = ANY($1)`, accounts)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string][]byte, len(accounts))
	for rows.Next() {
		var account string
		var raw []byte
		if err := rows.Scan(&account, &raw); err != nil {
			return nil, err
		}
		metadata, err := environment.ParsePasswordVaultMetadata(raw, s.issuer, account)
		if err != nil {
			return nil, err
		}
		out[account] = metadata.SharingPublicKey[:]
	}
	return out, rows.Err()
}

func currentKeyTx(ctx context.Context, tx *db.Tx, team string) (*recording.Key, error) {
	keys, err := keysTx(ctx, tx, team)
	if err != nil || len(keys) == 0 {
		return nil, err
	}
	return &keys[len(keys)-1], nil
}

// KeyState is GET /v1/teams/{team}/recording-key: any member reads it (holders seal for the pending viewers).
func (s *Service) KeyState(ctx context.Context, account, workspace string) (recording.KeyState, error) {
	out := recording.KeyState{Viewers: []recording.Viewer{}, ManualStart: recording.ManualStartAnyone}
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		team, _, err := scopeTx(ctx, tx, account, workspace, teams.PermissionTeamRead)
		if err != nil {
			return err
		}
		// Only the owner starts a recording on a personal machine.
		adminsOnly := isPersonalScope(team)
		if !adminsOnly {
			if err := tx.QueryRow(ctx, `SELECT recording_admins_only FROM teams WHERE team_id = $1`, team).Scan(&adminsOnly); err != nil {
				return err
			}
		}
		if adminsOnly {
			out.ManualStart = recording.ManualStartAdmins
		}
		key, err := currentKeyTx(ctx, tx, team)
		if err != nil {
			return err
		}
		out.Key = key
		viewers, err := viewersTx(ctx, tx, team)
		if err != nil {
			return err
		}
		sharing, err := s.sharingKeys(ctx, tx, viewers)
		if err != nil {
			return err
		}
		held := map[string][]byte{} // the sharing key each viewer's current-generation copy is sealed to
		if key != nil {
			rows, err := tx.Query(ctx, `SELECT account_id, recipient_sharing_public FROM recording_key_grants WHERE team_id = $1 AND generation = $2`, team, key.Generation)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var account string
				var to []byte
				if err := rows.Scan(&account, &to); err != nil {
					return err
				}
				held[account] = to
			}
			if err := rows.Err(); err != nil {
				return err
			}
		}
		for _, v := range viewers {
			viewer := recording.Viewer{AccountID: v, SharingPublicKey: sharing[v]}
			viewer.HasGrant = key != nil && sharing[v] != nil && bytes.Equal(held[v], sharing[v])
			out.Viewers = append(out.Viewers, viewer)
		}
		if key == nil {
			return nil
		}
		mine, err := s.viewerKeyTx(ctx, tx, account, team, key.Generation)
		if err == nil {
			out.MyKey = &mine
		} else if !errors.Is(err, ErrKeyPending) {
			return err
		}
		return nil
	})
	return out, err
}

// CreateKey is POST /v1/teams/{team}/recording-key (security admins): generation 1, a reset (the team owner, for a key nobody
// holds any more: signed by the owner's pinned reset key, no previous bundle, so older recordings stay unreadable), or a rotation signed by the
// current generation's signing key that carries the current private bundle sealed to the new key. It must seal
// for at least one viewer, so the key is never created with nobody holding it.
func (s *Service) CreateKey(ctx context.Context, account, workspace string, in recording.KeyCreate) (recording.Key, error) {
	k := in.Key
	reset := len(k.ResetSignature) != 0
	// A rotation carries the previous bundle sealed to the new key; a reset carries none (nobody holds it).
	if !k.ValidKey() || k.Generation > s.cfg.MaxKeyGenerations || len(in.Grants) == 0 || len(in.Grants) > 1000 ||
		(k.Generation == 1 || reset) != (len(in.SealedPrevious) == 0) || (k.Generation > 1 && !reset && !recording.ValidSealed(in.SealedPrevious)) {
		return recording.Key{}, ErrInvalid
	}
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		team, role, err := scopeTx(ctx, tx, account, workspace, teams.PermissionSecurityManage)
		if err != nil {
			return err
		}
		if err := teams.LockTeam(ctx, tx, team); err != nil {
			return err
		}
		current, err := currentKeyTx(ctx, tx, team)
		if err != nil {
			return err
		}
		owner, err := s.ownerWriterKey(ctx, tx, team)
		if err != nil {
			return err
		}
		switch {
		case current == nil && k.Generation != 1:
			return ErrKeyConflict
		case current == nil:
			// Generation 1 pins the owner's ENV writer key as the reset authority: what machines trust for a reset is the
			// key the ENV team-vault reset already trusts (the server's record of the owner's vault), pinned with the
			// first key exactly as generation 1 itself is. The server sets it; a client cannot name another.
			k.ResetPublicKey = owner
		case reset:
			// Only the team owner resets, and only with the key the previous generation pinned: the same signature a
			// machine checks, so what the server stores is what machines accept.
			if role != "owner" || isPersonalScope(team) {
				return ErrForbidden
			}
			if !recording.VerifyReset(team, *current, k) || owner == nil || !bytes.Equal(k.ResetPublicKey, current.ResetPublicKey) {
				return ErrKeyConflict
			}
		default:
			// A holder replaces the key; the reset authority it carries is signed over, and must stay what it was.
			if !recording.VerifyRotation(team, *current, k) || !bytes.Equal(k.ResetPublicKey, current.ResetPublicKey) {
				return ErrKeyConflict
			}
		}
		var previous []byte
		if k.Generation > 1 && !reset {
			previous = in.SealedPrevious
		}
		if _, err := tx.Exec(ctx, `INSERT INTO recording_keys(team_id, generation, public_key, signing_public_key, rotation_signature, sealed_previous, reset_public_key, reset_signature, created_by)
 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, team, k.Generation, k.PublicKey, k.SigningPublicKey, nilIfEmpty(k.RotationSignature), previous, nilIfEmpty(k.ResetPublicKey), nilIfEmpty(k.ResetSignature), account); err != nil {
			return err
		}
		if err := s.grantTx(ctx, tx, account, team, k.Generation, in.Grants, true); err != nil {
			return err
		}
		action := "recording_key_rotated"
		switch {
		case k.Generation == 1:
			action = "recording_key_created"
		case reset:
			action = "recording_key_reset"
		}
		if err := auditTx(ctx, tx, account, team, action, "recording-key-"+strconv.FormatInt(k.Generation, 10), map[string]any{"generation": k.Generation, "rows": len(in.Grants)}); err != nil {
			return err
		}
		return publishPolicyTx(ctx, tx, team)
	})
	return k, err
}

// PutGrants is PUT /v1/teams/{team}/recording-key/grants: a viewer holding the current generation seals it for
// viewers who have none (or whose vault changed). A viewer's valid copy is never replaced.
func (s *Service) PutGrants(ctx context.Context, account, workspace string, in recording.GrantsPut) error {
	if len(in.Grants) == 0 || len(in.Grants) > 1000 {
		return ErrInvalid
	}
	return s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		team, _, err := scopeTx(ctx, tx, account, workspace, teams.PermissionTeamRead)
		if err != nil {
			return err
		}
		if err := teams.LockTeam(ctx, tx, team); err != nil {
			return err
		}
		current, err := currentKeyTx(ctx, tx, team)
		if err != nil {
			return err
		}
		if current == nil {
			return ErrKeyUnavailable
		}
		if in.Generation != current.Generation {
			return ErrKeyConflict
		}
		var holds bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM recording_key_grants WHERE team_id = $1 AND account_id = $2 AND generation = $3)`, team, account, in.Generation).Scan(&holds); err != nil {
			return err
		}
		viewers, err := viewersTx(ctx, tx, team)
		if err != nil {
			return err
		}
		if !holds || !slices.Contains(viewers, account) {
			return ErrForbidden
		}
		if err := s.grantTx(ctx, tx, account, team, in.Generation, in.Grants, false); err != nil {
			return err
		}
		return auditTx(ctx, tx, account, team, "recording_key_granted", "recording-grants-"+strconv.FormatInt(s.now().UnixNano(), 10), map[string]any{"generation": in.Generation, "rows": len(in.Grants)})
	})
}

// grantTx stores sealed copies for viewers. Each recipient must be a viewer and each copy sealed to their current
// vault sharing key. replace also overwrites valid copies (a new generation replaces every copy it seals).
func (s *Service) grantTx(ctx context.Context, tx *db.Tx, account, team string, generation int64, grants []recording.Grant, replace bool) error {
	viewers, err := viewersTx(ctx, tx, team)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, g := range grants {
		if seen[g.AccountID] || !slices.Contains(viewers, g.AccountID) || !recording.ValidSealed(g.Sealed) {
			return ErrInvalid
		}
		seen[g.AccountID] = true
		sharing, err := s.sharingKeyTx(ctx, tx, g.AccountID)
		if err != nil {
			return err
		}
		if sharing == nil || !bytes.Equal(sharing, g.SharingPublicKey) {
			return ErrKeyConflict // the viewer's vault changed since the client read it
		}
		condition := `WHERE recording_key_grants.generation < EXCLUDED.generation OR recording_key_grants.recipient_sharing_public <> EXCLUDED.recipient_sharing_public`
		if replace {
			condition = ""
		}
		if _, err := tx.Exec(ctx, `INSERT INTO recording_key_grants(team_id, account_id, generation, recipient_sharing_public, sealed, granted_by) VALUES ($1, $2, $3, $4, $5, $6)
 ON CONFLICT (team_id, account_id) DO UPDATE SET generation = EXCLUDED.generation, recipient_sharing_public = EXCLUDED.recipient_sharing_public, sealed = EXCLUDED.sealed,
 granted_by = EXCLUDED.granted_by, created_at = now() `+condition, team, g.AccountID, generation, sharing, g.Sealed, account); err != nil {
			return err
		}
	}
	return nil
}

// PutSettings is PUT /v1/teams/{team}/recording-settings (settings admins).
func (s *Service) PutSettings(ctx context.Context, account, workspace string, in recording.Settings) error {
	if !recording.ValidManualStart(in.ManualStart) {
		return ErrInvalid
	}
	return s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		team, _, err := scopeTx(ctx, tx, account, workspace, teams.PermissionSettingsManage)
		if err != nil {
			return err
		}
		if isPersonalScope(team) {
			return ErrInvalid // nothing to set: only the owner starts a personal recording
		}
		if err := teams.LockTeam(ctx, tx, team); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE teams SET recording_admins_only = $2 WHERE team_id = $1 AND recording_admins_only <> $2`, team, in.ManualStart == recording.ManualStartAdmins)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		if err := auditTx(ctx, tx, account, team, "recording_settings_changed", "recording-settings-"+strconv.FormatInt(s.now().UnixNano(), 10), map[string]any{"manual_start": in.ManualStart}); err != nil {
			return err
		}
		return publishPolicyTx(ctx, tx, team)
	})
}

func nilIfEmpty(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
}

// ownerWriterKey is the team owner's ENV vault writer public key as the server records it (nil: the owner has no vault
// yet, so the key cannot be reset). A personal scope's owner is its account.
func (s *Service) ownerWriterKey(ctx context.Context, tx *db.Tx, team string) ([]byte, error) {
	owner := strings.TrimPrefix(team, personalPrefix)
	if !isPersonalScope(team) {
		if err := tx.QueryRow(ctx, `SELECT owner_account FROM teams WHERE team_id = $1`, team).Scan(&owner); err != nil {
			return nil, err
		}
	}
	return s.writerKey(ctx, tx, owner)
}

func (s *Service) vaultWriterKey(ctx context.Context, tx *db.Tx, account string) ([]byte, error) {
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT envelope FROM environment_password_vaults WHERE account_id = $1`, account).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	metadata, err := environment.ParsePasswordVaultMetadata(raw, s.issuer, account)
	if err != nil {
		return nil, err
	}
	return metadata.WriterPublicKey[:], nil
}
