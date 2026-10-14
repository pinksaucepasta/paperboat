package backupreport

import (
	"context"
	"time"
)

// HealthState is a fixed operational code, not an identifier or raw error.
type HealthState uint8

const (
	HealthUnknown HealthState = iota
	HealthCurrent
	HealthStale
	HealthFailed
	HealthExpired
	HealthNotConfigured
	HealthUnavailable
)

type EvidenceHealth struct {
	State      HealthState `json:"state"`
	AgeSeconds *int64      `json:"age_seconds,omitempty"`
}
type HealthSnapshot struct {
	Backup  EvidenceHealth `json:"backup"`
	Restore EvidenceHealth `json:"restore"`
}

// Health is exclusively process-owned telemetry. It never returns evidence
// identities, and does not replace the authenticated, audited Status API.
func (s *Service) Health(ctx context.Context) (HealthSnapshot, error) {
	out := HealthSnapshot{Backup: EvidenceHealth{State: HealthUnavailable}, Restore: EvidenceHealth{State: HealthUnavailable}}
	if s == nil {
		return out, nil
	}
	if s.cfg.Token == "" {
		out.Backup.State, out.Restore.State = HealthNotConfigured, HealthNotConfigured
		return out, nil
	}
	if s.store == nil {
		return out, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	rows, err := s.store.Pool().Query(ctx, `SELECT DISTINCT ON (kind) kind,outcome,completed_at,retained_until FROM backup_reports WHERE database_id=$1 AND kind IN ('backup','restore') ORDER BY kind,completed_at DESC,run_id DESC LIMIT 2`, s.cfg.DatabaseID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	observed := s.now()
	result := HealthSnapshot{Backup: EvidenceHealth{State: HealthUnknown}, Restore: EvidenceHealth{State: HealthUnknown}}
	for rows.Next() {
		var kind, outcome string
		var completed time.Time
		var retained *time.Time
		if err := rows.Scan(&kind, &outcome, &completed, &retained); err != nil {
			return out, err
		}
		r := &Record{Report: Report{Kind: kind, Outcome: outcome, CompletedAt: completed}}
		if retained != nil {
			r.RetainedUntil = *retained
		}
		if kind == "backup" {
			result.Backup = recordHealth(r, observed, s.cfg.MaxAge)
		} else {
			result.Restore = recordHealth(r, observed, s.cfg.RestoreMaxAge)
		}
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	return result, nil
}
func recordHealth(r *Record, now time.Time, maxAge time.Duration) EvidenceHealth {
	out := EvidenceHealth{State: HealthUnknown}
	if r == nil {
		return out
	}
	switch evidenceState(r, now, maxAge) {
	case "current":
		out.State = HealthCurrent
	case "stale":
		out.State = HealthStale
	case "failed":
		out.State = HealthFailed
	}
	if r.Kind == "backup" && r.Outcome == "succeeded" && !r.RetainedUntil.After(now) {
		out.State = HealthExpired
	}
	age := int64(now.Sub(r.CompletedAt) / time.Second)
	if age < 0 {
		age = 0
	}
	out.AgeSeconds = &age
	return out
}
