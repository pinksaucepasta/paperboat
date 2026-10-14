// Package backupreport records deployment-owned backup and restore evidence.
// A responsive database, archive checksum, or SDK acknowledgement is not a restore test.
package backupreport

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat-server/internal/audit"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
)

var (
	ErrInvalid     = errors.New("invalid backup report")
	ErrForbidden   = errors.New("backup authority required")
	ErrConflict    = errors.New("backup report conflicts with immutable evidence")
	ErrUnavailable = errors.New("backup reporting unavailable")
)

const EvidenceRetention = 90 * 24 * time.Hour
const maxReports = 10000

type Report struct {
	DatabaseID    string    `json:"database_id"`
	RunID         string    `json:"run_id"`
	Kind          string    `json:"kind"`
	BackupRunID   string    `json:"backup_run_id,omitempty"`
	StartedAt     time.Time `json:"started_at"`
	CompletedAt   time.Time `json:"completed_at"`
	Outcome       string    `json:"outcome"`
	FailureCode   string    `json:"failure_code,omitempty"`
	Bytes         int64     `json:"bytes"`
	SHA256        string    `json:"sha256,omitempty"`
	RetainedUntil time.Time `json:"retained_until"`
}
type Config struct {
	DatabaseID    string
	Token         string
	MaxAge        time.Duration
	RestoreMaxAge time.Duration
}
type Service struct {
	store *db.DB
	audit *audit.Writer
	cfg   Config
	now   func() time.Time
}

func NewService(store *db.DB, cfg Config) (*Service, error) {
	if cfg.DatabaseID == "" {
		cfg.DatabaseID = "primary"
	}
	if cfg.MaxAge == 0 {
		cfg.MaxAge = 26 * time.Hour
	}
	if cfg.RestoreMaxAge == 0 {
		cfg.RestoreMaxAge = 8 * 24 * time.Hour
	}
	if !identifier(cfg.DatabaseID, 64) || cfg.MaxAge < time.Minute || cfg.MaxAge > 30*24*time.Hour || cfg.RestoreMaxAge < time.Minute || cfg.RestoreMaxAge > EvidenceRetention || cfg.Token != "" && (len(cfg.Token) < 32 || len(cfg.Token) > 512) {
		return nil, ErrInvalid
	}
	return &Service{store: store, audit: audit.NewWriter(store), cfg: cfg, now: time.Now}, nil
}
func identifier(s string, max int) bool {
	if len(s) == 0 || len(s) > max {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}
func (s *Service) Authenticate(token string) bool {
	if s == nil || s.cfg.Token == "" || len(token) > 512 {
		return false
	}
	a := sha256.Sum256([]byte(token))
	b := sha256.Sum256([]byte(s.cfg.Token))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}
func validReport(r Report, now time.Time) bool {
	if !identifier(r.DatabaseID, 64) || !identifier(r.RunID, 128) || r.StartedAt.IsZero() || r.CompletedAt.Before(r.StartedAt) || r.CompletedAt.After(now.Add(time.Minute)) || r.StartedAt.Before(now.Add(-EvidenceRetention)) || (r.Outcome == "succeeded" && r.CompletedAt.Sub(r.StartedAt) > 48*time.Hour) || r.Bytes < 0 {
		return false
	}
	if r.Kind != "backup" && r.Kind != "restore" || r.Outcome != "succeeded" && r.Outcome != "failed" {
		return false
	}
	if r.Kind == "backup" && r.BackupRunID != "" || r.Kind == "restore" && (!identifier(r.BackupRunID, 128) || r.BackupRunID == r.RunID) {
		return false
	}
	digest, e := hex.DecodeString(r.SHA256)
	if r.SHA256 != "" && (e != nil || len(digest) != 32 || hex.EncodeToString(digest) != r.SHA256) {
		return false
	}
	if r.Outcome == "succeeded" && (r.FailureCode != "" || r.Bytes == 0 || r.SHA256 == "") {
		return false
	}
	if r.Outcome == "failed" {
		switch r.FailureCode {
		case "backup_failed", "archive_invalid":
			if r.Kind != "backup" {
				return false
			}
		case "restore_failed", "verification_cleanup_failed":
			if r.Kind != "restore" {
				return false
			}
		default:
			return false
		}
	}
	if r.Kind == "backup" && r.Outcome == "succeeded" && (!r.RetainedUntil.After(r.CompletedAt) || r.RetainedUntil.After(r.CompletedAt.Add(366*24*time.Hour))) {
		return false
	}
	if r.Kind == "restore" && !r.RetainedUntil.IsZero() {
		return false
	}
	if r.Kind == "backup" && r.Outcome == "failed" && (!r.RetainedUntil.IsZero() || r.Bytes != 0 || r.SHA256 != "") {
		return false
	}
	return true
}
func (s *Service) Ingest(ctx context.Context, token string, r Report) error {
	if !s.Authenticate(token) {
		return ErrForbidden
	}
	if s.store == nil {
		return ErrUnavailable
	}
	// Normalize PostgreSQL timestamp precision before request hashing/replays.
	r.StartedAt = r.StartedAt.UTC().Truncate(time.Microsecond)
	r.CompletedAt = r.CompletedAt.UTC().Truncate(time.Microsecond)
	r.RetainedUntil = r.RetainedUntil.UTC().Truncate(time.Microsecond)
	if r.DatabaseID != s.cfg.DatabaseID || !validReport(r, s.now()) {
		return ErrInvalid
	}
	raw, _ := json.Marshal(r)
	hash := sha256.Sum256(raw)
	return s.store.InReadCommittedTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,219))`, r.DatabaseID); err != nil {
			return err
		}
		var previous []byte
		err := tx.QueryRow(ctx, `SELECT request_hash FROM backup_reports WHERE database_id=$1 AND run_id=$2`, r.DatabaseID, r.RunID).Scan(&previous)
		if err == nil {
			if subtle.ConstantTimeCompare(previous, hash[:]) != 1 {
				return ErrConflict
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if r.Kind == "restore" {
			var digest string
			var size int64
			err = tx.QueryRow(ctx, `SELECT sha256,bytes FROM backup_reports WHERE database_id=$1 AND run_id=$2 AND kind='backup' AND outcome='succeeded' AND completed_at<=$3`, r.DatabaseID, r.BackupRunID, r.StartedAt).Scan(&digest, &size)
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrConflict
			}
			if err != nil {
				return err
			}
			if digest != r.SHA256 || size != r.Bytes {
				return ErrConflict
			}
		}
		if _, err = tx.Exec(ctx, `DELETE FROM backup_reports WHERE database_id=$1 AND received_at<$2`, r.DatabaseID, s.now().Add(-EvidenceRetention)); err != nil {
			return err
		}
		var count int
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM backup_reports WHERE database_id=$1`, r.DatabaseID).Scan(&count); err != nil {
			return err
		}
		if count >= maxReports {
			return ErrUnavailable
		}
		var retention any
		if !r.RetainedUntil.IsZero() {
			retention = r.RetainedUntil
		}
		_, err = tx.Exec(ctx, `INSERT INTO backup_reports(database_id,run_id,kind,backup_run_id,started_at,completed_at,outcome,failure_code,bytes,sha256,retained_until,request_hash) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`, r.DatabaseID, r.RunID, r.Kind, r.BackupRunID, r.StartedAt, r.CompletedAt, r.Outcome, r.FailureCode, r.Bytes, r.SHA256, retention, hash[:])
		if err != nil {
			return err
		}
		return s.audit.WriteTx(ctx, tx, audit.Event{ActorType: audit.ActorSystem, EventType: "backup.report_received", ResourceType: "database_backup", ResourceID: r.RunID, IdempotencyKey: r.DatabaseID + ":" + r.RunID, Metadata: map[string]any{"database_id": r.DatabaseID, "kind": r.Kind, "outcome": r.Outcome}})
	})
}

type Record struct {
	Report
	ReceivedAt time.Time `json:"received_at"`
}
type Status struct {
	DatabaseID                  string    `json:"database_id"`
	ReportingConfigured         bool      `json:"reporting_configured"`
	BackupState                 string    `json:"backup_state"`
	RestoreState                string    `json:"restore_state"`
	ObservedAt                  time.Time `json:"observed_at"`
	MaxAgeSeconds               int64     `json:"max_age_seconds"`
	RestoreMaxAgeSeconds        int64     `json:"restore_max_age_seconds"`
	EvidenceRetentionDays       int       `json:"evidence_retention_days"`
	LatestBackup                *Record   `json:"latest_backup,omitempty"`
	LatestRestore               *Record   `json:"latest_restore,omitempty"`
	LastSuccessfulBackup        *Record   `json:"last_successful_backup,omitempty"`
	LatestBackupRestoreVerified bool      `json:"latest_backup_restore_verified"`
	StorageDurability           string    `json:"storage_durability"`
}
type Page struct {
	Runs      []Record `json:"runs"`
	NextAfter string   `json:"next_after,omitempty"`
}

const columns = `database_id,run_id,kind,backup_run_id,started_at,completed_at,outcome,failure_code,bytes,sha256,retained_until,received_at`

type scanner interface{ Scan(...any) error }

func scan(row scanner) (*Record, error) {
	r := new(Record)
	var until *time.Time
	err := row.Scan(&r.DatabaseID, &r.RunID, &r.Kind, &r.BackupRunID, &r.StartedAt, &r.CompletedAt, &r.Outcome, &r.FailureCode, &r.Bytes, &r.SHA256, &until, &r.ReceivedAt)
	if until != nil {
		r.RetainedUntil = *until
	}
	return r, err
}
func (s *Service) read(ctx context.Context, actor, resource string, fn func(context.Context, *db.Tx) error) error {
	if s == nil || s.store == nil {
		return ErrUnavailable
	}
	return s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var allowed bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM operator_accounts WHERE id=$1 AND status='active' AND role IN ('admin','operations'))`, actor).Scan(&allowed); err != nil {
			return err
		}
		if !allowed {
			return ErrForbidden
		}
		if err := fn(ctx, tx); err != nil {
			return err
		}
		return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: actor, ActorType: audit.ActorAdmin, EventType: "operator.backups_read", ResourceType: "database_backup", ResourceID: resource})
	})
}
func (s *Service) Status(ctx context.Context, actor string) (Status, error) {
	var out Status
	err := s.read(ctx, actor, s.cfg.DatabaseID, func(ctx context.Context, tx *db.Tx) error {
		out = Status{DatabaseID: s.cfg.DatabaseID, ReportingConfigured: s.cfg.Token != "", BackupState: "unknown", RestoreState: "unknown", ObservedAt: s.now().UTC(), MaxAgeSeconds: int64(s.cfg.MaxAge.Seconds()), RestoreMaxAgeSeconds: int64(s.cfg.RestoreMaxAge.Seconds()), EvidenceRetentionDays: 90, StorageDurability: "deployment_owned_unverified"}
		for _, q := range []struct {
			kind    string
			success bool
			target  **Record
		}{{"backup", false, &out.LatestBackup}, {"restore", false, &out.LatestRestore}, {"backup", true, &out.LastSuccessfulBackup}} {
			row, err := scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM backup_reports WHERE database_id=$1 AND kind=$2 AND (NOT $3 OR outcome='succeeded') ORDER BY completed_at DESC,run_id DESC LIMIT 1`, s.cfg.DatabaseID, q.kind, q.success))
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if err == nil {
				*q.target = row
			}
		}
		out.BackupState = evidenceState(out.LatestBackup, out.ObservedAt, s.cfg.MaxAge)
		out.RestoreState = evidenceState(out.LatestRestore, out.ObservedAt, s.cfg.RestoreMaxAge)
		if out.LatestBackup != nil && out.LatestBackup.Outcome == "succeeded" && !out.LatestBackup.RetainedUntil.After(out.ObservedAt) {
			out.BackupState = "expired"
		}
		if s.cfg.Token == "" {
			out.BackupState = "not_configured"
			out.RestoreState = "not_configured"
		}
		out.LatestBackupRestoreVerified = out.BackupState == "current" && out.RestoreState == "current" && out.LatestRestore.BackupRunID == out.LatestBackup.RunID
		return nil
	})
	if err != nil {
		return Status{}, err
	}
	return out, nil
}
func evidenceState(r *Record, now time.Time, maxAge time.Duration) string {
	if r == nil {
		return "unknown"
	}
	if r.Outcome == "failed" {
		return "failed"
	}
	if now.Sub(r.CompletedAt) > maxAge {
		return "stale"
	}
	return "current"
}
func (s *Service) List(ctx context.Context, actor, after string, limit int) (Page, error) {
	if limit < 1 || limit > 100 || after != "" && !identifier(after, 128) {
		return Page{}, ErrInvalid
	}
	var out Page
	err := s.read(ctx, actor, s.cfg.DatabaseID, func(ctx context.Context, tx *db.Tx) error {
		out = Page{Runs: []Record{}}
		rows, err := tx.Query(ctx, `SELECT `+columns+` FROM backup_reports WHERE database_id=$1 AND ($2='' OR run_id>$2) ORDER BY run_id LIMIT $3`, s.cfg.DatabaseID, after, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scan(rows)
			if err != nil {
				return err
			}
			out.Runs = append(out.Runs, *r)
		}
		if err = rows.Err(); err != nil {
			return err
		}
		if len(out.Runs) > limit {
			out.Runs = out.Runs[:limit]
			out.NextAfter = out.Runs[limit-1].RunID
		}
		return nil
	})
	if err != nil {
		return Page{}, err
	}
	return out, nil
}
