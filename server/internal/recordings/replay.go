package recordings

import (
	"bytes"
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/admission"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/mint"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
	"github.com/pinksaucepasta/paperboat/shared/recording"
)

// Filter narrows List.
type Filter struct {
	MachineID string
	SessionID string
	Limit     int
	Offset    int
}

const maxListLimit = 200

// List is GET /v1/teams/{team}/recordings: owners and admins see every recording of the team, other members those
// on machines where they may replay.
func (s *Service) List(ctx context.Context, account, team string, f Filter) (recording.List, error) {
	if f.Limit == 0 {
		f.Limit = 50
	}
	if f.Limit < 1 || f.Limit > maxListLimit || f.Offset < 0 || len(f.MachineID) > 128 || len(f.SessionID) > 128 {
		return recording.List{}, ErrInvalid
	}
	out := recording.List{Recordings: []recording.Recording{}}
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		role, err := memberTx(ctx, tx, account, team, teams.PermissionTeamRead)
		if err != nil {
			return err
		}
		var machines []string // nil: every machine
		if role != "owner" && role != "admin" {
			if machines, err = textColumn(ctx, tx, `SELECT m FROM (SELECT DISTINCT machine_id AS m FROM session_recordings WHERE team_id = $1) d
 WHERE NOT isempty(paperboat.workspace_policy_rule_ports($2, $1, m, 'recording_view'))`, team, account); err != nil {
				return err
			}
		}
		where := `team_id = $1 AND ($2 = '' OR machine_id = $2) AND ($3 = '' OR session_id = $3) AND ($4::text[] IS NULL OR machine_id = ANY($4))`
		args := []any{team, f.MachineID, f.SessionID, machines}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM session_recordings WHERE `+where, args...).Scan(&out.Total); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id, machine_id, machine_name, session_id, trigger, coalesce(started_by, ''), accounts, started_at, ended_at, bytes, segments, state
 FROM session_recordings WHERE `+where+` ORDER BY started_at DESC, id LIMIT $5 OFFSET $6`, append(args, f.Limit, f.Offset)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r recording.Recording
			if err := rows.Scan(&r.ID, &r.MachineID, &r.MachineName, &r.SessionID, &r.Trigger, &r.StartedBy, &r.Accounts, &r.StartedAt, &r.EndedAt, &r.Bytes, &r.Segments, &r.State); err != nil {
				return err
			}
			out.Recordings = append(out.Recordings, r)
		}
		return rows.Err()
	})
	return out, err
}

// Replay is POST /v1/recordings/{id}/replay. In one transaction: the recording, the account's replay permission on
// its machine (an explicit recording_view rule), the viewer's sealed copy, then the replay audit event, and only
// then the ticket. Without the audit event there is no ticket; a ticket the transaction did not commit is never
// returned.
func (s *Service) Replay(ctx context.Context, account, id string, in recording.ReplayRequest) (recording.Replay, error) {
	if !recording.ValidRecordingID(id) {
		return recording.Replay{}, ErrNotFound
	}
	if in.BrowserPublicKeySHA256 != "" {
		// shortcut: the browser ticket (role "recording" on the browser transport) lands in the next change.
		return recording.Replay{}, ErrInvalid
	}
	var out recording.Replay
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var team, machine, session, state string
		err := tx.QueryRow(ctx, `SELECT team_id, machine_id, session_id, key_generation, state FROM session_recordings WHERE id = $1`, id).Scan(&team, &machine, &session, &out.KeyGeneration, &state)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if _, err := memberTx(ctx, tx, account, team, teams.PermissionTeamRead); err != nil {
			if errors.Is(err, ErrForbidden) {
				return ErrNotFound
			}
			return err
		}
		var allowed bool
		if err := tx.QueryRow(ctx, `SELECT NOT isempty(paperboat.workspace_policy_rule_ports($1, $2, $3, 'recording_view'))`, account, team, machine).Scan(&allowed); err != nil {
			return err
		}
		if !allowed {
			return ErrForbidden
		}
		if state == recording.StateDeleted {
			return ErrDeleted
		}
		var environmentID string
		err = tx.QueryRow(ctx, `SELECT environment_id FROM user_machines WHERE id = $1 AND deleted_at IS NULL AND revoked_at IS NULL`, machine).Scan(&environmentID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrDeleted // the machine, and the recording with it, is gone
		}
		if err != nil {
			return err
		}
		if out.ViewerKey, err = s.viewerKeyTx(ctx, tx, account, team, out.KeyGeneration); err != nil {
			return err
		}
		if out.Chain, err = chainTx(ctx, tx, team, out.KeyGeneration, out.ViewerKey.Generation); err != nil {
			return err
		}
		jti := "token_" + uuid.NewString()
		if err := teams.AuditTx(ctx, tx, account, team, "recording_replayed", jti, map[string]any{"recording_id": id, "machine_id": machine, "session_id": session, "generation": out.KeyGeneration}); err != nil {
			return err
		}
		issuer, err := admission.New(s.signer, recording.TicketLifetime)
		if err != nil {
			return err
		}
		now := s.now().UTC()
		credential, err := issuer.Issue(mint.CredentialInput{Issuer: s.issuer, Audience: "paperboat-machine", Subject: account, JTI: jti, IssuedAt: now, ExpiresAt: now.Add(recording.TicketLifetime),
			CredentialClass: recording.CredentialClass, Scopes: []string{recording.Scope}, EnvironmentID: environmentID, AccountID: account, UserID: account, WorkspaceID: team,
			MachineID: machine, ResourceKind: recording.ResourceKind, ResourceID: id})
		if err != nil {
			return err
		}
		out.Ticket, out.MachineID = credential.Token, machine
		return nil
	})
	if err != nil {
		return recording.Replay{}, err
	}
	return out, nil
}

// viewerKeyTx is account's sealed copy, still sealed to their current vault and new enough to open a recording of
// generation want.
func (s *Service) viewerKeyTx(ctx context.Context, tx *db.Tx, account, team string, want int64) (recording.SealedKey, error) {
	var k recording.SealedKey
	var sealedTo []byte
	err := tx.QueryRow(ctx, `SELECT generation, recipient_sharing_public, sealed FROM recording_key_grants WHERE team_id = $1 AND account_id = $2`, team, account).Scan(&k.Generation, &sealedTo, &k.Sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return k, ErrKeyPending
	}
	if err != nil {
		return k, err
	}
	current, err := s.sharingKeyTx(ctx, tx, account)
	if err != nil {
		return k, err
	}
	if current == nil || !bytes.Equal(current, sealedTo) || k.Generation < want {
		return k, ErrKeyPending
	}
	return k, nil
}

// chainTx lists the links from generation top down to from+1: each opens the generation below it.
func chainTx(ctx context.Context, tx *db.Tx, team string, from, top int64) ([]recording.ChainLink, error) {
	rows, err := tx.Query(ctx, `SELECT generation, sealed_previous FROM recording_keys WHERE team_id = $1 AND generation > $2 AND generation <= $3 ORDER BY generation DESC`, team, from, top)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []recording.ChainLink{}
	for rows.Next() {
		var l recording.ChainLink
		if err := rows.Scan(&l.Generation, &l.SealedPrevious); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if int64(len(out)) != top-from {
		return nil, ErrKeyUnavailable
	}
	return out, nil
}
