// Package abusereports keeps reports of abuse (phishing, malware, spam, illegal content) about links
// Paperboat serves. Anyone may report, no account needed (edge abuse plan 3.6.2); operators read
// them and take a link down with the publication_block action. Reports are kept 180 days; the
// writer deletes older ones, so the table stays bounded. No reporter IP is stored.
package abusereports

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
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
}

type Service struct {
	store       *db.DB
	now         func() time.Time
	mail        config.TransactionalMail
	mailEnabled bool
}

func New(store *db.DB) *Service {
	return &Service{store: store, now: func() time.Time { return time.Now().UTC() }}
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
	report := Report{ID: "abuse_" + uuid.NewString(), Hostname: host, URL: in.URL, Reason: in.Reason, Details: in.Details, ReporterEmail: in.ReporterEmail, CreatedAt: s.now()}
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

// List returns reports newest first, optionally for one hostname, older than before (zero: now).
func (s *Service) List(ctx context.Context, hostname string, before time.Time, limit int) ([]Report, error) {
	if limit < 1 || limit > 100 {
		return nil, ErrInvalid
	}
	if before.IsZero() {
		before = s.now().Add(time.Second)
	}
	rows, err := s.store.Pool().Query(ctx, `SELECT id,hostname,url,reason,details,reporter_email,created_at FROM abuse_reports
WHERE ($1='' OR hostname=$1) AND created_at<$2 ORDER BY created_at DESC, id DESC LIMIT $3`, strings.ToLower(strings.TrimSpace(hostname)), before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	reports := []Report{}
	for rows.Next() {
		var r Report
		if err := rows.Scan(&r.ID, &r.Hostname, &r.URL, &r.Reason, &r.Details, &r.ReporterEmail, &r.CreatedAt); err != nil {
			return nil, err
		}
		r.CreatedAt = r.CreatedAt.UTC()
		reports = append(reports, r)
	}
	return reports, rows.Err()
}

// alertMessage is the operator's copy of one report. Every reporter-supplied value is escaped.
func alertMessage(policy config.TransactionalMail, r Report) transactionalmail.Message {
	reporter := "not given"
	if r.ReporterEmail != "" {
		reporter = html.EscapeString(r.ReporterEmail)
	}
	return transactionalmail.Message{
		From: policy.From, To: []string{policy.AbuseMailbox},
		Subject: "Abuse report (" + r.Reason + "): " + r.Hostname,
		HTML: fmt.Sprintf("<p>Report %s, %s UTC.</p><p>Link: %s<br>Reason: %s<br>Reporter: %s</p><pre>%s</pre><p>Take it down: POST /v1/admin/publications/%s/block with Operator-Reason %s.</p>",
			r.ID, r.CreatedAt.Format("2006-01-02 15:04"), html.EscapeString(r.URL), r.Reason, reporter, html.EscapeString(r.Details), html.EscapeString(r.Hostname), r.ID),
	}
}
