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
	// IdempotencyRecords: replay records of retried requests (managed SSH operations, route, ENV vault and machine capability operations).
	IdempotencyRecords time.Duration `json:"idempotency_records"`
	// FinishedOperations: operations in a terminal state, which clients read back to reconcile a result.
	FinishedOperations time.Duration `json:"finished_operations"`
	// EndedSessions: access, CLI and browser sessions and device grants after they end. At least
	// UserMachines.ExpiredAccessSessionRetention, during which daemons may still name an ended access session.
	EndedSessions time.Duration `json:"ended_sessions"`
	// EndedPublications: a preview after it ended (`pb preview list --all` and the console's ended filter show it
	// until then), and a tunnel, tunnel route or preview/tunnel domain after it was deleted (past its quarantine), and a DNS
	// publication after its records were withdrawn.
	EndedPublications time.Duration `json:"ended_publications"`
	// FinishedEnrollments: a cancelled, expired, denied, revoked or deleted machine pairing, an installed pairing of a
	// removed or replaced machine, and a pairing reset receipt, once nothing can retry them (installation recovery lasts 24 hours, so at least that). Helper enrollment grants
	// go with ExpiredCredentials.
	FinishedEnrollments time.Duration `json:"finished_enrollments"`
	// AbandonedEnrollments: a machine pairing that failed and nobody retried (the console's retry button reads it).
	AbandonedEnrollments time.Duration `json:"abandoned_enrollments"`
	// AccountDeletionJobs: a finished (done or failed) provider job of an account deletion, the proof its cleanup ran.
	AccountDeletionJobs time.Duration `json:"account_deletion_jobs"`
	// UsageReceipts: edge usage receipts, by age. A receipt only answers a replay of the same operation (the edge
	// delivers a durable report up to accountingauthority.DeliveryWindow, 7 days, late), and a replay after deletion
	// adds no bytes (the counter keeps the greatest value), so twice the delivery window is enough.
	UsageReceipts          time.Duration `json:"usage_receipts"`
	TunnelLogs             time.Duration `json:"tunnel_logs"`
	DecidedMachineRequests time.Duration `json:"decided_machine_requests"`
	// InboxRequests: a team inbox request (and, by cascade, its receipt outbox row) after it ended (declined, expired,
	// revoked, consumed, completed) or after its expiry if nobody decided it.
	InboxRequests time.Duration `json:"inbox_requests"`
	// StripeEvents de-duplicate webhooks; Stripe retries for at most three days.
	StripeEvents time.Duration `json:"stripe_events"`
	// SupersededMeshEntries: a peer mesh log entry after a newer entry of the same endpoint replaced it (readers
	// read the latest entry per endpoint at a sequence they just took, so only a moment-old sequence can need it).
	SupersededMeshEntries time.Duration `json:"superseded_mesh_entries"`
	// Recordings: session recording index rows after the recording started (the bytes live on the machine, which
	// deletes them after Recordings.MachineRetentionDays).
	Recordings time.Duration `json:"recordings"`
	// NodeCapacityDaily: a node's per-day byte totals and peaks (and the latest-sample row of a node that stopped
	// reporting). At least 35 days, so a month's traffic is always complete.
	NodeCapacityDaily time.Duration `json:"node_capacity_daily"`
	// TrafficHourly: the hour totals of the operator traffic dashboard; past it they are folded into day totals, which
	// TrafficDaily deletes. TrafficHourly is at least 2 days (the "today" chart) and TrafficDaily at least 62 days, so
	// every period of the dashboard (today, 7 days, 30 days, this month) stays complete.
	TrafficHourly time.Duration `json:"traffic_hourly"`
	TrafficDaily  time.Duration `json:"traffic_daily"`
	// FleetDaily: the operator Fleet page's per-day version counts (fleet_version_daily). At least 35 days, so a rollout
	// chart of a month is complete.
	FleetDaily time.Duration `json:"fleet_daily"`
	// EndedAnnouncements: an operator announcement after it ended or was retracted (its receipts go with it).
	EndedAnnouncements time.Duration `json:"ended_announcements"`
	// DeletedTeams: what a deleted team held (members, invitations, settings, destinations and credentials) after the
	// team was deleted. The team's row, its name and its billing records stay, so history and audit events still read.
	DeletedTeams time.Duration `json:"deleted_teams"`
	// SupportReportsPerAccount keeps an account's newest reports (they also expire on their own).
	SupportReportsPerAccount int `json:"support_reports_per_account"`
	// Batch rows per delete statement and the time one pass may take, so a backlog drains without long locks.
	Batch  int           `json:"batch"`
	Budget time.Duration `json:"budget"`
}

func defaultRetention() Retention {
	return Retention{
		ExpiredCredentials: time.Hour, IdempotencyRecords: day, FinishedOperations: 7 * day, EndedSessions: 30 * day, EndedPublications: 30 * day,
		FinishedEnrollments: 7 * day, AbandonedEnrollments: 30 * day, AccountDeletionJobs: 90 * day,
		UsageReceipts: 14 * day, TunnelLogs: 7 * day, DecidedMachineRequests: 90 * day, InboxRequests: 30 * day, StripeEvents: 30 * day,
		SupersededMeshEntries: time.Hour, Recordings: 90 * day, NodeCapacityDaily: 90 * day, TrafficHourly: 8 * day, TrafficDaily: 400 * day, FleetDaily: 400 * day, EndedAnnouncements: 90 * day, DeletedTeams: 30 * day, SupportReportsPerAccount: 1000, Batch: 500, Budget: 20 * time.Second,
	}
}

func (r Retention) validate(accessSessionRetention time.Duration) error {
	switch {
	case r.ExpiredCredentials < time.Minute, r.IdempotencyRecords < time.Hour, r.FinishedOperations < day, r.EndedPublications < day, r.FinishedEnrollments < 2*day, r.AbandonedEnrollments < 2*day, r.AccountDeletionJobs < day,
		r.UsageReceipts < 7*day, r.TunnelLogs < time.Hour, r.DecidedMachineRequests < day, r.InboxRequests < day, r.StripeEvents < 4*day,
		r.SupersededMeshEntries < time.Minute, r.NodeCapacityDaily < 35*day, r.TrafficHourly < 2*day, r.TrafficDaily < 62*day, r.FleetDaily < 35*day, r.EndedAnnouncements < day, r.DeletedTeams < day:
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
