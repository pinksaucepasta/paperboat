package selfhost

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// ErrUpdateRunning means the node is already updating; one request at a time.
var ErrUpdateRunning = errors.New("this server is already updating; wait for it to finish")

// ErrUpdateNotRunning means a report for a request that is finished, replaced or another node's.
var ErrUpdateNotRunning = errors.New("this update is not running on this node")

// ErrUpdateUnavailable means no published self-host release to update to.
var ErrUpdateUnavailable = errors.New("no published self-host release")

// UpdateStatus is a node's latest update request as console and admin show it.
type UpdateStatus struct {
	ID               string    `json:"id"`
	TargetVersion    string    `json:"target_version"`
	State            string    `json:"state"`
	Detail           string    `json:"detail,omitempty"`
	SupportReference string    `json:"support_reference,omitempty"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// UpdateSteps a node reports, in order; succeeded, rolled_back and failed end a request.
var updateSteps = map[string]bool{"downloading": false, "verifying": false, "swapping": false, "checking": false, "succeeded": true, "rolled_back": true, "failed": true}

const finishedUpdateStates = `('succeeded','rolled_back','failed')`

// RequestUpdate asks the node behind an installation to update to target at its next check-in. Only someone who
// may change the installation (its scope's owner or admin; an operator for global nodes) may ask.
func (s *Service) RequestUpdate(ctx context.Context, actor, installationID string, operator bool, target string) (UpdateStatus, error) {
	var out UpdateStatus
	if actor == "" {
		return out, ErrUnauthorized
	}
	if installationID == "" || len(installationID) > 128 || !utf8.ValidString(installationID) || target == "" || len(target) > 64 {
		return out, ErrInvalid
	}
	var scope Scope
	var node string
	err := s.database.SQL().QueryRowContext(ctx, `SELECT scope_kind,scope_id,node_id FROM paperboat.selfhost_installations WHERE id=$1 AND state='active'`, installationID).Scan(&scope.Kind, &scope.ID, &node)
	if errors.Is(err, sql.ErrNoRows) {
		return out, ErrInstallationNotFound
	}
	if err != nil {
		return out, err
	}
	if operator != (scope.Kind == "global") {
		return out, ErrInstallationNotFound
	}
	if _, err = s.authorizeScope(ctx, actor, scope, true, operator); err != nil {
		return out, err
	}
	now := s.now().UTC()
	err = s.database.SQL().QueryRowContext(ctx, `INSERT INTO paperboat.selfhost_update_requests(node_id,id,target_version,requested_by,state,created_at,updated_at)
VALUES($1,$2,$3,$4,'requested',$5,$5)
ON CONFLICT (node_id) DO UPDATE SET id=EXCLUDED.id,target_version=EXCLUDED.target_version,requested_by=EXCLUDED.requested_by,
 state='requested',detail='',support_reference='',created_at=EXCLUDED.created_at,updated_at=EXCLUDED.updated_at
WHERE selfhost_update_requests.state IN `+finishedUpdateStates+`
RETURNING id,target_version,state,updated_at`, node, "upd_"+uuid.NewString(), target, actor, now).Scan(&out.ID, &out.TargetVersion, &out.State, &out.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return UpdateStatus{}, ErrUpdateRunning
	}
	return out, err
}

// runtimeNode is the node a runtime credential belongs to (the same rule as RuntimeReadiness).
func (s *Service) runtimeNode(ctx context.Context, credential string) (string, error) {
	if len(credential) < 32 || len(credential) > 4096 {
		return "", ErrUnauthorized
	}
	var node string
	err := s.database.SQL().QueryRowContext(ctx, `SELECT i.node_id FROM paperboat.selfhost_installations i
WHERE i.runtime_hash=$1 AND i.state='active' AND i.claim_complete AND (i.expires_at IS NULL OR i.expires_at>$2) LIMIT 1`, digest(credential), s.now().UTC()).Scan(&node)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrUnauthorized
	}
	return node, err
}

// UpdateCheck is a node's check-in: the update it should run, or nil.
func (s *Service) UpdateCheck(ctx context.Context, credential string) (*UpdateStatus, error) {
	node, err := s.runtimeNode(ctx, credential)
	if err != nil {
		return nil, err
	}
	var out UpdateStatus
	err = s.database.SQL().QueryRowContext(ctx, `SELECT id,target_version,state,updated_at FROM paperboat.selfhost_update_requests
WHERE node_id=$1 AND state NOT IN `+finishedUpdateStates, node).Scan(&out.ID, &out.TargetVersion, &out.State, &out.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateReport records one step of the node's running update. A report for another node's request or for a
// finished one is refused (ErrUpdateNotRunning), so a late or replayed report cannot reopen it.
func (s *Service) UpdateReport(ctx context.Context, credential, requestID, state, detail, supportReference string) error {
	if _, known := updateSteps[state]; !known || requestID == "" || len(requestID) > 128 || len(detail) > 1024 || len(supportReference) > 128 ||
		!utf8.ValidString(detail) || strings.ContainsRune(detail, '\x00') || !utf8.ValidString(supportReference) {
		return ErrInvalid
	}
	node, err := s.runtimeNode(ctx, credential)
	if err != nil {
		return err
	}
	var target string
	err = s.database.SQL().QueryRowContext(ctx, `UPDATE paperboat.selfhost_update_requests SET state=$3,detail=$4,support_reference=$5,updated_at=$6
WHERE node_id=$1 AND id=$2 AND state NOT IN `+finishedUpdateStates+` RETURNING target_version`, node, requestID, state, detail, supportReference, s.now().UTC()).Scan(&target)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrUpdateNotRunning
	}
	if err != nil || !updateSteps[state] {
		return err
	}
	// The step is recorded; a bell notice that fails to write is logged, never a reason for the node to report again.
	if err := s.notifyUpdateFinished(ctx, node, requestID, state, target, detail, supportReference); err != nil {
		slog.Warn("selfhost_update_notice_failed", "node", node, "request", requestID, "error", err)
	}
	return nil
}
