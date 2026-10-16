package resourceusage

import (
	"context"
	"time"

	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

func recordNativeReportingStatus(ctx context.Context, tx *db.Tx, machine string, generation int64, batch NativeBatch, rejected int, now time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO native_usage_reporting_status AS status
 (machine_id,installation_generation,workspace_id,last_seen,unrecorded_bytes,
 expired_bytes,rejected_reports,recording_incomplete)
 SELECT id,$2,coalesce('team:'||owner_team_id,'personal:'||user_id),$3,$4,$5,$6,$7
 FROM user_machines WHERE id=$1 AND installation_generation=$2
 ON CONFLICT(machine_id) DO UPDATE SET
 installation_generation=EXCLUDED.installation_generation,workspace_id=EXCLUDED.workspace_id,
 last_seen=EXCLUDED.last_seen,
 unrecorded_bytes=CASE WHEN status.installation_generation=EXCLUDED.installation_generation
 AND status.workspace_id=EXCLUDED.workspace_id
 THEN greatest(status.unrecorded_bytes,EXCLUDED.unrecorded_bytes) ELSE EXCLUDED.unrecorded_bytes END,
 expired_bytes=CASE WHEN status.installation_generation=EXCLUDED.installation_generation
 AND status.workspace_id=EXCLUDED.workspace_id
 THEN greatest(status.expired_bytes,EXCLUDED.expired_bytes) ELSE EXCLUDED.expired_bytes END,
 rejected_reports=CASE WHEN status.installation_generation=EXCLUDED.installation_generation
 AND status.workspace_id=EXCLUDED.workspace_id
 THEN status.rejected_reports+EXCLUDED.rejected_reports ELSE EXCLUDED.rejected_reports END,
 recording_incomplete=CASE WHEN status.installation_generation=EXCLUDED.installation_generation
 AND status.workspace_id=EXCLUDED.workspace_id
 THEN status.recording_incomplete OR EXCLUDED.recording_incomplete ELSE EXCLUDED.recording_incomplete END`,
		machine, generation, now, batch.UnrecordedBytes, batch.ExpiredBytes, int64(rejected), batch.RecordingIncomplete)
	return err
}
