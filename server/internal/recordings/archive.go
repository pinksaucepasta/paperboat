package recordings

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/teambucket"
	"github.com/pinksaucepasta/paperboat/shared/recording"
	shared "github.com/pinksaucepasta/paperboat/shared/teambucket"
)

// ErrNoBucket: the team has no bucket, or did not choose it for recordings.
var ErrNoBucket = errors.New("the team has no bucket for recordings")

// ErrNotArchived: no segment of the recording is in the bucket.
var ErrNotArchived = errors.New("the recording is not archived in the team bucket")

// Buckets opens a team's bucket inside the caller's transaction (teambucket.Service).
type Buckets interface {
	StoreFor(ctx context.Context, tx *db.Tx, team string, use teambucket.Use) (teambucket.ObjectStore, error)
}

// SetBuckets connects the service to the team buckets; without it the archive endpoints answer ErrNoBucket.
func (s *Service) SetBuckets(b Buckets) { s.buckets = b }

func (s *Service) storeTx(ctx context.Context, tx *db.Tx, team string, use teambucket.Use) (teambucket.ObjectStore, error) {
	if s.buckets == nil {
		return nil, ErrNoBucket
	}
	store, err := s.buckets.StoreFor(ctx, tx, team, use)
	if errors.Is(err, teambucket.ErrNotFound) || errors.Is(err, teambucket.ErrUseOff) {
		return nil, ErrNoBucket
	}
	return store, err
}

// UploadURLs is POST /v1/machine/recordings/{id}/upload-urls: one presigned upload per closed segment the machine
// names, bound to its size and SHA-256, for the machine that owns the recording only.
func (s *Service) UploadURLs(ctx context.Context, machineID, id string, in shared.UploadRequest) (shared.UploadURLs, error) {
	if !recording.ValidRecordingID(id) || len(in.Segments) < 1 || len(in.Segments) > shared.MaxSegmentsPerBatch {
		return shared.UploadURLs{}, ErrInvalid
	}
	for _, segment := range in.Segments {
		if segment.Validate() != nil {
			return shared.UploadURLs{}, ErrInvalid
		}
	}
	var out shared.UploadURLs
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var team, state string
		err := tx.QueryRow(ctx, `SELECT r.team_id, r.state FROM session_recordings r JOIN teams t ON t.team_id = r.team_id
 WHERE r.id = $1 AND r.machine_id = $2 AND t.deleted_at IS NULL AND NOT t.suspended AND team_paid_entitled(t.team_id)`, id, machineID).Scan(&team, &state)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if state == recording.StateDeleted {
			return ErrDeleted
		}
		store, err := s.storeTx(ctx, tx, team, teambucket.UseRecordings)
		if err != nil {
			return err
		}
		for _, segment := range in.Segments {
			key, err := shared.RecordingSegmentKey(store.Config().Prefix, id, segment.Index)
			if err != nil {
				return ErrInvalid
			}
			upload, err := store.PresignPut(ctx, key, segment.Bytes, segment.SHA256, shared.URLLifetime)
			if err != nil {
				return err
			}
			out.Uploads = append(out.Uploads, upload)
		}
		return nil
	})
	return out, err
}

// Archived is POST /v1/machine/recordings/{id}/archived: the machine's first n segments are in the bucket. It only
// ever grows, and only the recording's own machine moves it.
func (s *Service) Archived(ctx context.Context, machineID, id string, n int) error {
	if !recording.ValidRecordingID(id) || n < 0 || n > 1_000_000 {
		return ErrInvalid
	}
	return s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE session_recordings SET archived_segments = GREATEST(archived_segments, $3) WHERE id = $1 AND machine_id = $2`, id, machineID, n)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// maxArchiveSegments bounds one archive replay answer (one presigned URL each).
const maxArchiveSegments = 4096

// archiveReplayTx fills out for an archive replay of recording id (already authorized) and returns whether the
// archive holds every segment of an ended recording.
func (s *Service) archiveReplayTx(ctx context.Context, tx *db.Tx, t target, id string, out *recording.Replay) error {
	var archived, segments int64
	var state string
	if err := tx.QueryRow(ctx, `SELECT archived_segments, segments, state FROM session_recordings WHERE id = $1`, id).Scan(&archived, &segments, &state); err != nil {
		return err
	}
	if archived < 1 {
		return ErrNotArchived
	}
	if archived > maxArchiveSegments {
		return fmt.Errorf("%w: archive of %d segments exceeds one replay", ErrInvalid, archived)
	}
	store, err := s.storeTx(ctx, tx, t.team, teambucket.UseAny)
	if err != nil {
		return err
	}
	out.Source = recording.SourceBucket
	for i := 0; i < int(archived); i++ {
		key, err := shared.RecordingSegmentKey(store.Config().Prefix, id, i)
		if err != nil {
			return err
		}
		get, err := store.PresignGet(ctx, key, shared.URLLifetime)
		if err != nil {
			return err
		}
		out.Segments = append(out.Segments, get)
	}
	out.Final = state != recording.StateRecording && archived >= segments
	return nil
}

// PruneArchive deletes, from the team bucket, the segments of recordings that started more than
// config.Recordings.BucketRetentionDays ago, a bounded batch per call. An object that cannot be deleted keeps its
// row for the next pass; a team whose bucket is gone has nothing left for Paperboat to delete.
func (s *Service) PruneArchive(ctx context.Context) error {
	type expired struct {
		id, team string
		segments int
	}
	var batch []expired
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, team_id, archived_segments FROM session_recordings
 WHERE archived_segments > 0 AND started_at < $1 ORDER BY started_at LIMIT 50`, s.now().Add(-time.Duration(s.cfg.BucketRetentionDays)*24*time.Hour))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e expired
			var n int64
			if err := rows.Scan(&e.id, &e.team, &n); err != nil {
				return err
			}
			e.segments = int(min(n, 1_000_000))
			batch = append(batch, e)
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	var failures []error
	for _, e := range batch {
		var store teambucket.ObjectStore
		if err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) (err error) {
			store, err = s.storeTx(ctx, tx, e.team, teambucket.UseAny)
			return err
		}); err != nil && !errors.Is(err, ErrNoBucket) {
			failures = append(failures, err)
			continue
		} else if err == nil {
			deleted := true
			for i := 0; i < e.segments; i++ {
				key, err := shared.RecordingSegmentKey(store.Config().Prefix, e.id, i)
				if err == nil {
					err = store.Delete(ctx, key)
				}
				if err != nil {
					failures = append(failures, fmt.Errorf("delete archived segment %d of %s: %w", i, e.id, err))
					deleted = false
					break
				}
			}
			if !deleted {
				continue
			}
		}
		if err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE session_recordings SET archived_segments = 0 WHERE id = $1`, e.id)
			return err
		}); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
