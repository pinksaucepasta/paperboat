// Package abusereports keeps reports of abuse (phishing, malware, spam, illegal content) about links
// Paperboat serves. Anyone may report, no account needed (edge abuse plan 3.6.2); operators read
// them and take a link down with the publication_block action. Reports are kept 180 days; the
// writer deletes older ones, so the table stays bounded. No reporter IP is stored.
package abusereports

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/transactionalmail"
)

// ErrInvalid reports a report the server cannot accept; the message says which field.
var ErrInvalid = errors.New("abuse report invalid")

// Retention is how long a report is kept.
const Retention = 180 * 24 * time.Hour

// Reasons are the report categories, in the order a form lists them.
var Reasons = []string{"phishing", "malware", "spam", "illegal", "other"}

func validReason(r string) bool { return slices.Contains(Reasons, r) }

// Statuses are an operator's triage of a report; every report starts open.
var Statuses = []string{"open", "actioned", "dismissed"}

// ErrNotFound is a report that does not exist (or was pruned past retention).
var ErrNotFound = errors.New("abuse report not found")

// ErrForbidden is an actor who is not an active operations or admin operator.
var ErrForbidden = errors.New("abuse report triage forbidden")

type Input struct {
	URL           string `json:"url"`
	Reason        string `json:"reason"`
	Details       string `json:"details,omitempty"`
	ReporterEmail string `json:"reporter_email,omitempty"`
}

type Report struct {
	ID            string    `json:"id"`
	Hostname      string    `json:"hostname"`
	URL           string    `json:"url"`
	Reason        string    `json:"reason"`
	Details       string    `json:"details"`
	ReporterEmail string    `json:"reporter_email"`
	CreatedAt     time.Time `json:"created_at"`
	// Triage: Status is open until an operator actions (takes the hostname down) or dismisses it.
	Status     string     `json:"status"`
	TriagedBy  string     `json:"triaged_by,omitempty"`
	TriagedAt  *time.Time `json:"triaged_at,omitempty"`
	TriageNote string     `json:"triage_note,omitempty"`
}

type Service struct {
	audit       *audit.Writer
	store       *db.DB
	now         func() time.Time
	mail        config.TransactionalMail
	mailEnabled bool
}

func New(store *db.DB) *Service {
	return &Service{store: store, audit: audit.NewWriter(store), now: func() time.Time { return time.Now().UTC() }}
}

// ConfigureMail turns on the operator alert: every report is mailed to the abuse mailbox
// (mail.abuse_mailbox) in the same transaction, so no report goes unseen. Without mail or a mailbox,
// reports are only stored.
func (s *Service) ConfigureMail(policy config.TransactionalMail, enabled bool) {
	s.mail, s.mailEnabled = policy, enabled
}

// validate checks a report and returns its normalized form and hostname.
func validate(in Input) (Input, string, error) {
	in.URL, in.Reason = strings.TrimSpace(in.URL), strings.TrimSpace(in.Reason)
	in.Details, in.ReporterEmail = strings.TrimSpace(in.Details), strings.TrimSpace(in.ReporterEmail)
	u, err := url.Parse(in.URL)
	if err != nil || len(in.URL) > 2048 || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || hasControl(in.URL) {
		return Input{}, "", fmt.Errorf("%w: %s", ErrInvalid, "url must be the http or https address of the link")
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if len(host) > 253 {
		return Input{}, "", fmt.Errorf("%w: %s", ErrInvalid, "url host is too long")
	}
	known := false
	for _, r := range Reasons {
		known = known || r == in.Reason
	}
	if !known {
		return Input{}, "", fmt.Errorf("%w: reason must be one of %s", ErrInvalid, strings.Join(Reasons, ", "))
	}
	if len(in.Details) > 4000 || hasControlExceptNewline(in.Details) {
		return Input{}, "", fmt.Errorf("%w: %s", ErrInvalid, "details must be at most 4000 characters of text")
	}
	if in.ReporterEmail != "" && (len(in.ReporterEmail) > 320 || !strings.Contains(in.ReporterEmail, "@") || hasControl(in.ReporterEmail) || strings.ContainsAny(in.ReporterEmail, " <>,;")) {
		return Input{}, "", fmt.Errorf("%w: %s", ErrInvalid, "reporter_email must be one email address")
	}
	return in, host, nil
}

func hasControl(s string) bool { return strings.IndexFunc(s, unicode.IsControl) >= 0 }

func hasControlExceptNewline(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' }) >= 0
}

// Submit records one report and prunes reports past retention.
func (s *Service) Submit(ctx context.Context, in Input) (Report, error) {
	in, host, err := validate(in)
	if err != nil {
		return Report{}, err
	}
	report := Report{ID: "abuse_" + uuid.NewString(), Hostname: host, URL: in.URL, Reason: in.Reason, Details: in.Details, ReporterEmail: in.ReporterEmail, CreatedAt: s.now(), Status: "open"}
	err = s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO abuse_reports(id,hostname,url,reason,details,reporter_email,created_at) VALUES($1,$2,$3,$4,$5,$6,$7)`,
			report.ID, report.Hostname, report.URL, report.Reason, report.Details, report.ReporterEmail, report.CreatedAt); err != nil {
			return err
		}
		if s.mailEnabled && s.mail.AbuseMailbox != "" {
			if _, err := transactionalmail.EnqueueOnceTx(ctx, tx, "abuse-report:"+report.ID, alertMessage(s.mail, report)); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `DELETE FROM abuse_reports WHERE created_at<$1`, report.CreatedAt.Add(-Retention))
		return err
	})
	if err != nil {
		return Report{}, err
	}
	return report, nil
}

// Page is one page of reports, newest first. The next page starts before (NextBefore, NextBeforeID): created_at
// alone would skip reports filed in the same instant.
type Page struct {
	Total        int64      `json:"total"`
	Reports      []Report   `json:"reports"`
	NextBefore   *time.Time `json:"next_before,omitempty"`
	NextBeforeID string     `json:"next_before_id,omitempty"`
	// NextCursor is (NextBefore, NextBeforeID) in one opaque value, for pagers that carry a single cursor.
	NextCursor string `json:"next_cursor,omitempty"`
}

// ParseCursor reads a NextCursor back into its time and id.
func ParseCursor(cursor string) (time.Time, string, error) {
	at, id, ok := strings.Cut(cursor, ",")
	parsed, err := time.Parse(time.RFC3339Nano, at)
	if !ok || err != nil || id == "" || len(id) > 64 {
		return time.Time{}, "", ErrInvalid
	}
	return parsed, id, nil
}

// List returns reports newest first, optionally for one hostname, one reason and one status, older than
// (before, beforeID) (zero: now).
func (s *Service) List(ctx context.Context, hostname, reason, status string, before time.Time, beforeID string, limit int) (Page, error) {
	if limit < 1 || limit > 100 || len(beforeID) > 64 || (reason != "" && !validReason(reason)) || (status != "" && !slices.Contains(Statuses, status)) {
		return Page{}, ErrInvalid
	}
	if before.IsZero() {
		before, beforeID = s.now().Add(time.Second), ""
	}
	host := strings.ToLower(strings.TrimSpace(hostname))
	out := Page{Reports: []Report{}}
	if err := s.store.Pool().QueryRow(ctx, `SELECT count(*) FROM abuse_reports WHERE ($1='' OR hostname=$1) AND ($2='' OR reason=$2) AND ($3='' OR status=$3)`, host, reason, status).Scan(&out.Total); err != nil {
		return Page{}, err
	}
	rows, err := s.store.Pool().Query(ctx, `SELECT `+reportColumns+` FROM abuse_reports
WHERE ($1='' OR hostname=$1) AND ($5='' OR reason=$5) AND ($6='' OR status=$6) AND (created_at<$2 OR ($3<>'' AND created_at=$2 AND id<$3))
ORDER BY created_at DESC, id DESC LIMIT $4`, host, before, beforeID, limit+1, reason, status)
	if err != nil {
		return Page{}, err
	}
	defer rows.Close()
	for rows.Next() {
		r, err := scanReport(rows)
		if err != nil {
			return Page{}, err
		}
		if len(out.Reports) == limit {
			last := out.Reports[limit-1]
			out.NextBefore, out.NextBeforeID = &last.CreatedAt, last.ID
			out.NextCursor = last.CreatedAt.UTC().Format(time.RFC3339Nano) + "," + last.ID
			break
		}
		r.CreatedAt = r.CreatedAt.UTC()
		out.Reports = append(out.Reports, r)
	}
	return out, rows.Err()
}

// alertMessage is the operator's copy of one report. Every reporter-supplied value is escaped.
func alertMessage(policy config.TransactionalMail, r Report) transactionalmail.Message {
	reporter := "not given"
	if r.ReporterEmail != "" {
		reporter = r.ReporterEmail
	}
	return transactionalmail.Compose(policy, policy.AbuseMailbox, "Abuse report ("+r.Reason+"): "+r.Hostname, transactionalmail.Content{
		Heading:    "Abuse report " + r.ID,
		Paragraphs: []string{fmt.Sprintf("Received %s UTC. Take it down: POST /v1/admin/publications/%s/block with Operator-Reason %s.", r.CreatedAt.Format("2006-01-02 15:04"), r.Hostname, r.ID)},
		Details:    []transactionalmail.Detail{{Label: "Link", Value: r.URL}, {Label: "Reason", Value: r.Reason}, {Label: "Reporter", Value: reporter}, {Label: "Details", Value: r.Details}},
		Why:        "You got this because this address receives abuse reports.",
	})
}

const reportColumns = `id,hostname,url,reason,details,reporter_email,created_at,status,coalesce(triaged_by,''),triaged_at,triage_note`

func scanReport(row interface{ Scan(...any) error }) (Report, error) {
	var r Report
	var at *time.Time
	err := row.Scan(&r.ID, &r.Hostname, &r.URL, &r.Reason, &r.Details, &r.ReporterEmail, &r.CreatedAt, &r.Status, &r.TriagedBy, &at, &r.TriageNote)
	r.CreatedAt = r.CreatedAt.UTC()
	if at != nil {
		utc := at.UTC()
		r.TriagedAt = &utc
	}
	return r, err
}

// Triage sets a report's status (open, actioned or dismissed) with a note, recording who and when, audited in the
// same transaction with the operator's reference. Reopening clears the note, who and when.
func (s *Service) Triage(ctx context.Context, actor, id, status, note, reference string) (Report, error) {
	note = strings.TrimSpace(note)
	if actor == "" || id == "" || len(id) > 64 || !slices.Contains(Statuses, status) || len(note) > 1000 || hasControlExceptNewline(note) {
		return Report{}, ErrInvalid
	}
	var out Report
	err := s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var role, state string
		if err := tx.QueryRow(ctx, `SELECT role,status FROM operator_accounts WHERE id=$1`, actor).Scan(&role, &state); err != nil || state != "active" || (role != "admin" && role != "operations") {
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			return ErrForbidden
		}
		var err error
		out, err = scanReport(tx.QueryRow(ctx, `UPDATE abuse_reports SET status=$2, triage_note=CASE WHEN $2='open' THEN '' ELSE $3 END,
  triaged_by=CASE WHEN $2='open' THEN NULL ELSE $4 END, triaged_at=CASE WHEN $2='open' THEN NULL ELSE now() END
 WHERE id=$1 RETURNING `+reportColumns, id, status, note, actor))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: actor, ActorType: audit.ActorAdmin, EventType: "abuse_report.triaged", ResourceType: "abuse_report", ResourceID: id, Metadata: map[string]any{"status": status, "note": note, "reference": reference, "hostname": out.Hostname}})
	})
	if err != nil {
		return Report{}, err
	}
	return out, nil
}
