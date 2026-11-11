// Package recordings is the server side of Teams session recordings (shared/recording): each team machine's
// recording policy, the recording index fed by the machines' audit backlog, the team recording key with every
// viewer's sealed copy, and replay tickets issued only after the replay audit event is written. Recording bytes
// never reach the server.
package recordings

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/admission"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/shared/recording"
)

var (
	ErrNotFound       = errors.New("recording not found")
	ErrForbidden      = errors.New("recording access is not allowed")
	ErrNeedsTeam      = errors.New("session recordings are part of the Teams plan")
	ErrInvalid        = errors.New("recording request is invalid")
	ErrKeyUnavailable = errors.New("the team has no recording key")
	ErrKeyPending     = errors.New("no sealed copy of the recording key for this viewer yet")
	ErrKeyConflict    = errors.New("the recording key changed or the rotation is not signed by the current key")
	ErrDeleted        = errors.New("the recording was deleted from its machine")
	ErrMachineGone    = errors.New("the machine identity is no longer current")
	ErrTooMany        = errors.New("too many replays at once; wait a few minutes")
)

type Service struct {
	db     *db.DB
	cfg    config.Recordings
	signer admission.Signer
	// issuer signs replay tickets and names the password vaults viewers' copies are sealed to.
	issuer string
	now    func() time.Time
	// sharingKeys maps each account to its current vault sharing public key; an account without a vault is absent
	// (vaultSharingKeys; tests stub it).
	sharingKeys func(context.Context, *db.Tx, []string) (map[string][]byte, error)
}

func New(store *db.DB, cfg config.Recordings, signer admission.Signer, issuer string) *Service {
	s := &Service{db: store, cfg: cfg, signer: signer, issuer: issuer, now: time.Now}
	s.sharingKeys = s.vaultSharingKeys
	return s
}

// subjectMatch is the policy subject test of workspace_policy_rule_ports for rule r and member a (account_id, role).
const subjectMatch = `(r.subject_kind = 'all'
   OR (r.subject_kind = 'account' AND r.subject_id = a.account_id)
   OR (r.subject_kind = 'role' AND (r.subject_id = 'member' OR r.subject_id = a.role OR (r.subject_id = 'admin' AND a.role = 'owner')))
   OR (r.subject_kind = 'group' AND EXISTS(SELECT 1 FROM workspace_policy_groups g WHERE g.policy_id = r.policy_id AND g.group_name = r.subject_id AND g.account_id = a.account_id)))`

const members = `members AS (
  SELECT m.account_id, CASE WHEN t.owner_account = m.account_id THEN 'owner' ELSE m.role END AS role
  FROM teams t JOIN team_members m USING (team_id) WHERE t.team_id = $1 AND m.active)`

// recordedSQL lists the members of team $1 whose terminal sessions on machine $2 are recorded: an allow record rule
// on the machine, every machine or a tag holding it, and no deny (deny wins, as in shared/policy Decide).
const recordedSQL = `WITH p AS (SELECT id FROM workspace_policies WHERE team_id = $1),
 targeted AS (
  SELECT r.* FROM workspace_policy_rules r JOIN p ON r.policy_id = p.id WHERE r.action = 'record' AND r.target_kind = 'machine' AND r.target_id = $2
  UNION ALL
  SELECT r.* FROM workspace_policy_rules r JOIN p ON r.policy_id = p.id WHERE r.action = 'record' AND r.target_kind = 'all_machines'
  UNION ALL
  SELECT r.* FROM workspace_policy_rules r JOIN p ON r.policy_id = p.id WHERE r.action = 'record' AND r.target_kind = 'tag'
   AND EXISTS(SELECT 1 FROM workspace_policy_tags tg WHERE tg.policy_id = p.id AND tg.tag = r.target_id AND tg.machine_id = $2)
 ), ` + members + `
 SELECT a.account_id FROM targeted r JOIN members a ON ` + subjectMatch + `
 GROUP BY a.account_id HAVING NOT bool_or(r.deny) ORDER BY a.account_id`

// MachinePolicy is GET /v1/machine/recording-policy for machineID (from the verified machine proof).
func (s *Service) MachinePolicy(ctx context.Context, machineID string) (recording.Policy, error) {
	out := recording.Policy{Keys: []recording.Key{}, RecordAccounts: []string{}, AdminAccounts: []string{}, ManualStart: recording.ManualStartAnyone,
		RetentionDays: s.cfg.MachineRetentionDays, DiskQuotaMB: s.cfg.DiskQuotaMB}
	err := s.db.InReadOnlySnapshot(ctx, func(ctx context.Context, tx *db.Tx) error {
		var team *string
		var enabled, adminsOnly bool
		err := tx.QueryRow(ctx, `SELECT m.owner_team_id, coalesce(t.deleted_at IS NULL AND NOT t.suspended AND team_paid_entitled(t.team_id), false), coalesce(t.recording_admins_only, false)
 FROM user_machines m LEFT JOIN teams t ON t.team_id = m.owner_team_id WHERE m.id = $1 AND m.deleted_at IS NULL AND m.revoked_at IS NULL`, machineID).Scan(&team, &enabled, &adminsOnly)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrMachineGone
		}
		if err != nil || team == nil || !enabled {
			return err
		}
		out.Enabled, out.TeamID = true, *team
		if adminsOnly {
			out.ManualStart = recording.ManualStartAdmins
		}
		if out.Keys, err = keysTx(ctx, tx, *team); err != nil {
			return err
		}
		if out.RecordAccounts, err = textColumn(ctx, tx, recordedSQL, *team, machineID); err != nil {
			return err
		}
		out.AdminAccounts, err = textColumn(ctx, tx, `SELECT owner_account FROM teams WHERE team_id = $1
 UNION SELECT account_id FROM team_members WHERE team_id = $1 AND active AND role = 'admin' ORDER BY 1`, *team)
		return err
	})
	if err != nil {
		return recording.Policy{}, err
	}
	out.Version = version(out)
	return out, nil
}

// version is a digest of the policy content, so it changes exactly when the content does.
func version(p recording.Policy) int64 {
	p.Version = 0
	raw, _ := json.Marshal(p)
	sum := sha256.Sum256(raw)
	return int64(binary.BigEndian.Uint64(sum[:8]) >> 1)
}

func keysTx(ctx context.Context, tx *db.Tx, team string) ([]recording.Key, error) {
	rows, err := tx.Query(ctx, `SELECT generation, public_key, signing_public_key, rotation_signature FROM recording_keys WHERE team_id = $1 ORDER BY generation`, team)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []recording.Key{}
	for rows.Next() {
		var k recording.Key
		if err := rows.Scan(&k.Generation, &k.PublicKey, &k.SigningPublicKey, &k.RotationSignature); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func textColumn(ctx context.Context, tx *db.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
