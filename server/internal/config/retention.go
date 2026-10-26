package config

import (
	"fmt"
	"time"
)

// Retention bounds the tables that grow with traffic (PLAN 3b, storage). One place for every window; the
// retention worker (internal/retention) deletes past them in bounded batches.
type Retention struct {
	// ExpiredCredentials: issued once-only credentials and redemptions past their expiry (config credentials live
	// 2 minutes; the revocation document lists only unexpired ones).
	ExpiredCredentials time.Duration `json:"expired_credentials"`
	// IdempotencyRecords: replay records of retried requests (managed SSH operations).
	IdempotencyRecords time.Duration `json:"idempotency_records"`
	// FinishedOperations: operations in a terminal state, which clients read back to reconcile a result.
	FinishedOperations time.Duration `json:"finished_operations"`
	// EndedSessions: access, CLI and browser sessions and device grants after they end. At least
	// UserMachines.ExpiredAccessSessionRetention, during which daemons may still name an ended access session.
	EndedSessions time.Duration `json:"ended_sessions"`
	// AcknowledgedUsageReceipts: edge usage receipts after the server acknowledged them.
	AcknowledgedUsageReceipts time.Duration `json:"acknowledged_usage_receipts"`
	TunnelLogs                time.Duration `json:"tunnel_logs"`
	DecidedMachineRequests    time.Duration `json:"decided_machine_requests"`
	// StripeEvents de-duplicate webhooks; Stripe retries for at most three days.
	StripeEvents time.Duration `json:"stripe_events"`
	// SupersededMeshEntries: a peer mesh log entry after a newer entry of the same endpoint replaced it (readers
	// read the latest entry per endpoint at a sequence they just took, so only a moment-old sequence can need it).
	SupersededMeshEntries time.Duration `json:"superseded_mesh_entries"`
	// SupportReportsPerAccount keeps an account's newest reports (they also expire on their own).
	SupportReportsPerAccount int `json:"support_reports_per_account"`
	// Batch rows per delete statement and the time one pass may take, so a backlog drains without long locks.
	Batch  int           `json:"batch"`
	Budget time.Duration `json:"budget"`
}

func defaultRetention() Retention {
	return Retention{
		ExpiredCredentials: time.Hour, IdempotencyRecords: day, FinishedOperations: 7 * day, EndedSessions: 30 * day,
		AcknowledgedUsageReceipts: 2 * day, TunnelLogs: 7 * day, DecidedMachineRequests: 90 * day, StripeEvents: 30 * day,
		SupersededMeshEntries: time.Hour, SupportReportsPerAccount: 1000, Batch: 500, Budget: 20 * time.Second,
	}
}

func (r Retention) validate(accessSessionRetention time.Duration) error {
	switch {
	case r.ExpiredCredentials < time.Minute, r.IdempotencyRecords < time.Hour, r.FinishedOperations < day,
		r.AcknowledgedUsageReceipts < time.Hour, r.TunnelLogs < time.Hour, r.DecidedMachineRequests < day, r.StripeEvents < 4*day,
		r.SupersededMeshEntries < time.Minute:
		return fmt.Errorf("retention: a window is below its minimum")
	case r.EndedSessions < accessSessionRetention:
		return fmt.Errorf("retention.ended_sessions must be at least user_machines.expired_access_session_retention")
	case r.SupportReportsPerAccount < 10:
		return fmt.Errorf("retention.support_reports_per_account must be at least 10")
	case r.Batch < 1 || r.Batch > 10_000, r.Budget < time.Second || r.Budget > 5*time.Minute:
		return fmt.Errorf("retention: batch must be 1-10000 and budget 1 s-5 min")
	}
	return nil
}
