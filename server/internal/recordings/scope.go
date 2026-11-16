package recordings

import (
	"context"
	"strings"

	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
	"github.com/pinksaucepasta/paperboat/shared/recording"
)

// A personal workspace (Free and Pro) records under the scope "personal:<account>" in every team_id column of the
// recording tables: its key, its copy, its index rows. A team id never holds ':', so the two never meet. The owner is
// the only viewer and the only one who starts or replays; no policy rules, no plan check, no bucket.
const (
	personalWorkspace = recording.PersonalWorkspace
	personalPrefix    = personalWorkspace + ":"
)

func personalScope(account string) string { return recording.PersonalScope(account) }

func isPersonalScope(scope string) bool { return recording.IsPersonalScope(scope) }

// scopeTx resolves the workspace of a /v1/teams/{team}/ request: the team after memberTx's checks, or, for the
// workspace "personal", the caller's own scope. The role of a personal scope is owner.
func scopeTx(ctx context.Context, tx *db.Tx, account, workspace string, permission teams.Permission) (scope, role string, err error) {
	if workspace == personalWorkspace {
		return personalScope(account), "owner", nil
	}
	role, err = memberTx(ctx, tx, account, workspace, permission)
	return workspace, role, err
}

// viewersTx lists who holds the key of scope: the owner of a personal scope, else the team policy's viewers.
func viewersTx(ctx context.Context, tx *db.Tx, scope string) ([]string, error) {
	if isPersonalScope(scope) {
		return []string{strings.TrimPrefix(scope, personalPrefix)}, nil
	}
	return textColumn(ctx, tx, `SELECT paperboat.recording_viewers($1) ORDER BY 1`, scope)
}

// auditTx writes a recording audit event: the team's activity feed, or the owner's personal one.
func auditTx(ctx context.Context, tx *db.Tx, account, scope, action, operation string, metadata map[string]any) error {
	if !isPersonalScope(scope) {
		return teams.AuditTx(ctx, tx, account, scope, action, operation, metadata)
	}
	return audit.NewWriter(nil).WriteTx(ctx, tx, audit.Event{ActorUserID: account, ActorType: audit.ActorUser, EventType: "personal." + action,
		ResourceType: "personal_recording", ResourceID: account, IdempotencyKey: account + ":" + operation, Metadata: metadata})
}

// publishPolicyTx tells the machines that record under scope to refetch their recording policy.
func publishPolicyTx(ctx context.Context, tx *db.Tx, scope string) error {
	if !isPersonalScope(scope) {
		_, err := tx.Exec(ctx, `SELECT paperboat.publish_team_recording_policy(ARRAY[$1::text])`, scope)
		return err
	}
	_, err := tx.Exec(ctx, `SELECT paperboat.publish_machine_topic(coalesce((SELECT array_agg(m.id) FROM user_machines m
 WHERE m.user_id = $1 AND m.deleted_at IS NULL AND m.revoked_at IS NULL AND paperboat.recording_team(m.id) IS NULL), '{}'::text[]), 'recording_policy')`, strings.TrimPrefix(scope, personalPrefix))
	return err
}
