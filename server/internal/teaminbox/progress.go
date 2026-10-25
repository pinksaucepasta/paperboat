package teaminbox

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Progress is the sender's report on a transfer it started from an approved request. The sender
// reports at most every few seconds; the server also skips an unchanged report within a second.
type Progress struct {
	TransferMode   string `json:"transfer_mode"`
	TransferState  string `json:"transfer_state"`
	PauseReason    string `json:"pause_reason,omitempty"`
	CommittedBytes int64  `json:"committed_bytes"`
}

var (
	transferModes  = map[string]bool{"automatic": true, "unlimited": true}
	transferStates = map[string]bool{"sending": true, "paused": true, "done": true, "failed": true}
	pauseReasons   = map[string]bool{"no_eligible_path": true, "destination_offline": true, "relay_quota_exhausted": true}
)

const progressMinInterval = time.Second

func (p Progress) valid() bool {
	return transferModes[p.TransferMode] && transferStates[p.TransferState] && p.CommittedBytes >= 0 &&
		(p.TransferState == "paused") == pauseReasons[p.PauseReason] && (p.TransferState == "paused" || p.PauseReason == "")
}

// ReportProgress records the sender's progress on its request. The mode is fixed by the first
// report, committed bytes never go back and never pass the manifest total, and only an approved
// request being sent (approved, consumed or completed) takes progress.
func (s *Service) ReportProgress(ctx context.Context, sender, requestID string, p Progress) (Request, error) {
	if !safeID.MatchString(sender) || !safeID.MatchString(requestID) || !p.valid() {
		return Request{}, ErrInvalid
	}
	var reason sql.NullString
	if p.PauseReason != "" {
		reason = sql.NullString{String: p.PauseReason, Valid: true}
	}
	result, err := s.db.SQL().ExecContext(ctx, `UPDATE paperboat.team_inbox_requests r
SET transfer_mode=$3,transfer_state=$4,pause_reason=$5,committed_bytes=$6,progress_at=$7,updated_at=$7
WHERE r.request_id=$2 AND r.sender_account=$1 AND r.status IN ('approved','consumed','completed')
  AND (r.transfer_mode IS NULL OR r.transfer_mode=$3) AND r.committed_bytes<=$6
  AND $6<=(SELECT coalesce(sum((f->>'size')::bigint),0) FROM jsonb_array_elements(r.manifest) f)
  AND NOT (r.transfer_state IS NOT DISTINCT FROM $4 AND r.pause_reason IS NOT DISTINCT FROM $5
       AND r.committed_bytes=$6 AND r.progress_at>$7::timestamptz-$8::interval)`,
		sender, requestID, p.TransferMode, p.TransferState, reason, p.CommittedBytes, s.now().UTC(), progressMinInterval.String())
	if err != nil {
		return Request{}, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return Request{}, err
	}
	current, err := s.Get(ctx, sender, requestID)
	if err != nil || n == 1 {
		return current, err
	}
	// Nothing changed: either an unchanged report inside the interval, or a refused one.
	switch {
	case current.SenderAccount != sender:
		return Request{}, ErrNotFound
	case current.TransferState == p.TransferState && current.PauseReason == p.PauseReason && current.CommittedBytes == p.CommittedBytes && current.TransferMode == p.TransferMode:
		return current, nil
	case current.Status != "approved" && current.Status != "consumed" && current.Status != "completed":
		return Request{}, ErrConflict
	case current.TransferMode != "" && current.TransferMode != p.TransferMode, p.CommittedBytes < current.CommittedBytes, p.CommittedBytes > current.TotalBytes:
		return Request{}, ErrConflict
	default:
		return Request{}, errors.New("team inbox progress was not recorded")
	}
}
