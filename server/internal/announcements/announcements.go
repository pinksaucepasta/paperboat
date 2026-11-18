// Package announcements is what operators tell users: a notice (a row in the console bell) or a banner (the ribbon above
// every page), for everyone, a plan, a team or one user, between a start and an optional end. A user's read of a notice and
// dismissal of a banner are kept per user. Rows are deleted by retention a while after they end or are retracted.
package announcements

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

var (
	ErrInvalid  = errors.New("invalid announcement")
	ErrNotFound = errors.New("announcement not found")
	// ErrConflict: another banner for the same audience runs in that window, or the key was used for a different change.
	ErrConflict = errors.New("announcement conflict")
)

const (
	KindNotice = "notice"
	KindBanner = "banner"

	AudienceAll  = "all"
	AudiencePlan = "plan"
	AudienceTeam = "team"
	AudienceUser = "user"

	// FreePlan is the audience of accounts without a paid plan.
	FreePlan = "free"

	MaxTitle = 80
	MaxBody  = 280
	maxLink  = 512
)

// Announcement is one row, as the operator sees it.
type Announcement struct {
	ID            string     `json:"id"`
	Kind          string     `json:"kind"`
	Title         string     `json:"title"`
	Body          string     `json:"body"`
	LinkURL       string     `json:"link_url,omitempty"`
	Severity      string     `json:"severity"`
	Dismissible   bool       `json:"dismissible"`
	AudienceKind  string     `json:"audience_kind"`
	AudienceValue string     `json:"audience_value,omitempty"`
	StartsAt      time.Time  `json:"starts_at"`
	EndsAt        *time.Time `json:"ends_at,omitempty"`
	CreatedBy     string     `json:"created_by"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	RetractedAt   *time.Time `json:"retracted_at,omitempty"`
	// State is scheduled, live, ended or retracted, by the database's clock.
	State string `json:"state"`
	// Reads counts the users who read a notice; Dismissals those who dismissed a banner.
	Reads      int64 `json:"reads"`
	Dismissals int64 `json:"dismissals"`
}

// Input is what an operator writes. StartsAt empty starts now; Kind cannot change after creation.
type Input struct {
	Kind          string     `json:"kind"`
	Title         string     `json:"title"`
	Body          string     `json:"body"`
	LinkURL       string     `json:"link_url"`
	Severity      string     `json:"severity"`
	Dismissible   *bool      `json:"dismissible"`
	AudienceKind  string     `json:"audience_kind"`
	AudienceValue string     `json:"audience_value"`
	StartsAt      *time.Time `json:"starts_at"`
	EndsAt        *time.Time `json:"ends_at"`
}

type Service struct {
	store *db.DB
	audit *audit.Writer
}

func New(store *db.DB) *Service { return &Service{store: store, audit: audit.NewWriter(store)} }

const columns = `id,kind,title,body,coalesce(link_url,''),severity,dismissible,audience_kind,audience_value,starts_at,ends_at,created_by,created_at,updated_at,retracted_at,` +
	`CASE WHEN retracted_at IS NOT NULL THEN 'retracted' WHEN ends_at IS NOT NULL AND ends_at<=now() THEN 'ended' WHEN starts_at>now() THEN 'scheduled' ELSE 'live' END,` +
	`(SELECT count(*) FROM paperboat.announcement_receipts r WHERE r.announcement_id=announcements.id AND r.read_at IS NOT NULL),` +
	`(SELECT count(*) FROM paperboat.announcement_receipts r WHERE r.announcement_id=announcements.id AND r.dismissed_at IS NOT NULL)`

func scan(row pgx.Row) (Announcement, error) {
	var a Announcement
	err := row.Scan(&a.ID, &a.Kind, &a.Title, &a.Body, &a.LinkURL, &a.Severity, &a.Dismissible, &a.AudienceKind, &a.AudienceValue,
		&a.StartsAt, &a.EndsAt, &a.CreatedBy, &a.CreatedAt, &a.UpdatedAt, &a.RetractedAt, &a.State, &a.Reads, &a.Dismissals)
	if errors.Is(err, pgx.ErrNoRows) {
		return Announcement{}, ErrNotFound
	}
	return a, err
}

// normalize checks an operator's input and fills defaults. Notices carry no severity; a ribbon is dismissible unless said otherwise.
func normalize(in Input) (Input, error) {
	in.Title, in.Body, in.LinkURL = strings.TrimSpace(in.Title), strings.TrimSpace(in.Body), strings.TrimSpace(in.LinkURL)
	in.AudienceValue = strings.TrimSpace(in.AudienceValue)
	if in.Kind != KindNotice && in.Kind != KindBanner {
		return in, fmt.Errorf("%w: kind is notice or banner", ErrInvalid)
	}
	if n := utf8.RuneCountInString(in.Title); n < 1 || n > MaxTitle {
		return in, fmt.Errorf("%w: the title is 1 to %d characters", ErrInvalid, MaxTitle)
	}
	if utf8.RuneCountInString(in.Body) > MaxBody {
		return in, fmt.Errorf("%w: the text is at most %d characters", ErrInvalid, MaxBody)
	}
	if in.LinkURL != "" && !validLink(in.LinkURL) {
		return in, fmt.Errorf("%w: the link is an https address or a console path", ErrInvalid)
	}
	switch in.Severity {
	case "":
		in.Severity = "info"
	case "info", "warning", "critical":
	default:
		return in, fmt.Errorf("%w: severity is info, warning or critical", ErrInvalid)
	}
	if in.Kind == KindNotice {
		in.Severity = "info"
	}
	if in.Dismissible == nil {
		yes := true
		in.Dismissible = &yes
	}
	switch in.AudienceKind {
	case AudienceAll:
		in.AudienceValue = ""
	case AudiencePlan, AudienceTeam, AudienceUser:
		if in.AudienceValue == "" || len(in.AudienceValue) > 128 {
			return in, fmt.Errorf("%w: name the %s", ErrInvalid, in.AudienceKind)
		}
	default:
		return in, fmt.Errorf("%w: the audience is all, plan, team or user", ErrInvalid)
	}
	if in.StartsAt != nil && in.EndsAt != nil && !in.EndsAt.After(*in.StartsAt) {
		return in, fmt.Errorf("%w: it must end after it starts", ErrInvalid)
	}
	return in, nil
}

// validLink: https only, or a path inside the console ("//host" would leave it).
func validLink(link string) bool {
	if len(link) > maxLink || strings.ContainsAny(link, " \t\r\n") {
		return false
	}
	if strings.HasPrefix(link, "/") {
		return link == "/" || !strings.HasPrefix(link, "//") && !strings.Contains(link, `\`)
	}
	u, err := url.Parse(link)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil
}

// audienceExists refuses a team, user or plan that does not exist: a typo would otherwise reach nobody, silently.
func audienceExists(ctx context.Context, tx *db.Tx, kind, value string) error {
	var query string
	switch kind {
	case AudienceUser:
		query = `SELECT EXISTS(SELECT 1 FROM paperboat.users WHERE id=$1)`
	case AudienceTeam:
		query = `SELECT EXISTS(SELECT 1 FROM paperboat.teams WHERE team_id=$1 AND deleted_at IS NULL)`
	case AudiencePlan:
		query = `SELECT $1='` + FreePlan + `' OR EXISTS(SELECT 1 FROM paperboat.billing_products WHERE code=$1)`
	default:
		return nil
	}
	var ok bool
	if err := tx.QueryRow(ctx, query, value).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: no such %s %q", ErrInvalid, kind, value)
	}
	return nil
}

// oneBanner refuses a banner whose window overlaps another banner of the same audience (one ribbon at a time per audience).
// starts nil is now, by the database's clock (the one that decides what is live).
func oneBanner(ctx context.Context, tx *db.Tx, id string, in Input, starts *time.Time) error {
	if in.Kind != KindBanner {
		return nil
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('announcements.banner', 0))`); err != nil {
		return err
	}
	var other string
	err := tx.QueryRow(ctx, `SELECT title FROM paperboat.announcements WHERE kind='banner' AND retracted_at IS NULL AND id<>$1
 AND audience_kind=$2 AND audience_value=$3 AND (ends_at IS NULL OR ends_at>coalesce($4::timestamptz,now())) AND ($5::timestamptz IS NULL OR starts_at<$5) AND (ends_at IS NULL OR ends_at>now()) LIMIT 1`,
		id, in.AudienceKind, in.AudienceValue, starts, in.EndsAt).Scan(&other)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: the banner %q already runs for this audience in that time", ErrConflict, other)
}

// sameWrite: a retry of a create carries what the row was created with (edits since then are not a different request).
func sameWrite(a Announcement, in Input) bool {
	return a.Kind == in.Kind && a.Title == in.Title && a.AudienceKind == in.AudienceKind && a.AudienceValue == in.AudienceValue
}

func (s *Service) record(ctx context.Context, tx *db.Tx, actor, key, event string, a Announcement) error {
	return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: actor, ActorType: audit.ActorAdmin, EventType: "operator." + event, ResourceType: "announcement", ResourceID: a.ID, IdempotencyKey: key,
		Metadata: map[string]any{"kind": a.Kind, "audience_kind": a.AudienceKind, "audience_value": a.AudienceValue, "starts_at": a.StartsAt, "ends_at": a.EndsAt}})
}

// Create writes an announcement. A retried key returns the row it created.
func (s *Service) Create(ctx context.Context, actor, key string, in Input) (Announcement, error) {
	in, err := normalize(in)
	if err != nil {
		return Announcement{}, err
	}
	var out Announcement
	err = s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		out, err = scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM paperboat.announcements WHERE created_by=$1 AND idempotency_key=$2`, actor, key))
		if err == nil {
			if !sameWrite(out, in) {
				return fmt.Errorf("%w: this Idempotency-Key was used for a different announcement", ErrConflict)
			}
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		if err = audienceExists(ctx, tx, in.AudienceKind, in.AudienceValue); err != nil {
			return err
		}
		if in.EndsAt != nil && !in.EndsAt.After(time.Now()) {
			return fmt.Errorf("%w: it ends in the past; retract it to end it now", ErrInvalid)
		}
		id := "ann_" + uuid.NewString()
		if err = oneBanner(ctx, tx, id, in, in.StartsAt); err != nil {
			return err
		}
		out, err = scan(tx.QueryRow(ctx, `INSERT INTO paperboat.announcements(id,kind,title,body,link_url,severity,dismissible,audience_kind,audience_value,starts_at,ends_at,created_by,idempotency_key)
 VALUES($1,$2,$3,$4,nullif($5,''),$6,$7,$8,$9,coalesce($10::timestamptz,now()),$11,$12,$13) RETURNING `+columns, id, in.Kind, in.Title, in.Body, in.LinkURL, in.Severity, *in.Dismissible, in.AudienceKind, in.AudienceValue, in.StartsAt, in.EndsAt, actor, key))
		if err != nil {
			return err
		}
		return s.record(ctx, tx, actor, key, "announcement_create", out)
	})
	return out, err
}

// Update replaces what an operator wrote of a running or scheduled announcement; its kind stays.
func (s *Service) Update(ctx context.Context, actor, key, id string, in Input) (Announcement, error) {
	in, err := normalize(in)
	if err != nil {
		return Announcement{}, err
	}
	var out Announcement
	err = s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var kind string
		var retracted bool
		var starts time.Time
		err := tx.QueryRow(ctx, `SELECT kind,retracted_at IS NOT NULL,starts_at FROM paperboat.announcements WHERE id=$1 FOR UPDATE`, id).Scan(&kind, &retracted, &starts)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if retracted || kind != in.Kind {
			return fmt.Errorf("%w: a retracted announcement or its kind cannot change", ErrConflict)
		}
		starting := &starts
		if in.StartsAt != nil {
			starting = in.StartsAt
		}
		if in.EndsAt != nil && !in.EndsAt.After(*starting) {
			return fmt.Errorf("%w: it must end after it starts", ErrInvalid)
		}
		if in.EndsAt != nil && !in.EndsAt.After(time.Now()) {
			return fmt.Errorf("%w: it ends in the past; retract it to end it now", ErrInvalid)
		}
		if err = audienceExists(ctx, tx, in.AudienceKind, in.AudienceValue); err != nil {
			return err
		}
		if err = oneBanner(ctx, tx, id, in, starting); err != nil {
			return err
		}
		out, err = scan(tx.QueryRow(ctx, `UPDATE paperboat.announcements SET title=$2,body=$3,link_url=nullif($4,''),severity=$5,dismissible=$6,audience_kind=$7,audience_value=$8,starts_at=$9,ends_at=$10,updated_at=now()
 WHERE id=$1 RETURNING `+columns, id, in.Title, in.Body, in.LinkURL, in.Severity, *in.Dismissible, in.AudienceKind, in.AudienceValue, *starting, in.EndsAt))
		if err != nil {
			return err
		}
		return s.record(ctx, tx, actor, key, "announcement_update", out)
	})
	return out, err
}

// Retract ends an announcement now for everyone; retracting twice is the same as once (and audited once).
func (s *Service) Retract(ctx context.Context, actor, key, id string) (Announcement, error) {
	var out Announcement
	err := s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		var already bool
		if err := tx.QueryRow(ctx, `SELECT retracted_at IS NOT NULL FROM paperboat.announcements WHERE id=$1 FOR UPDATE`, id).Scan(&already); errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		var err error
		if already {
			out, err = scan(tx.QueryRow(ctx, `SELECT `+columns+` FROM paperboat.announcements WHERE id=$1`, id))
			return err
		}
		out, err = scan(tx.QueryRow(ctx, `UPDATE paperboat.announcements SET retracted_at=now(),retracted_by=$2,updated_at=now() WHERE id=$1 RETURNING `+columns, id, actor))
		if err != nil {
			return err
		}
		return s.record(ctx, tx, actor, key, "announcement_retract", out)
	})
	return out, err
}

// Page is one page of the operator's list, newest first.
type Page struct {
	Announcements []Announcement `json:"announcements"`
	NextAfter     string         `json:"next_after,omitempty"`
	Total         int64          `json:"total"`
}

// List pages announcements by sequence, newest first; state filters (scheduled, live, ended, retracted; empty is all).
func (s *Service) List(ctx context.Context, state string, after int64, limit int) (Page, error) {
	switch state {
	case "", "scheduled", "live", "ended", "retracted":
	default:
		return Page{}, ErrInvalid
	}
	const stateOf = `CASE WHEN retracted_at IS NOT NULL THEN 'retracted' WHEN ends_at IS NOT NULL AND ends_at<=now() THEN 'ended' WHEN starts_at>now() THEN 'scheduled' ELSE 'live' END`
	page := Page{Announcements: []Announcement{}}
	err := s.store.Pool().QueryRow(ctx, `SELECT count(*) FROM paperboat.announcements WHERE ($1='' OR `+stateOf+`=$1)`, state).Scan(&page.Total)
	if err != nil {
		return Page{}, err
	}
	rows, err := s.store.Pool().Query(ctx, `SELECT `+columns+`,seq FROM paperboat.announcements WHERE ($1='' OR `+stateOf+`=$1) AND ($2=0 OR seq<$2) ORDER BY seq DESC LIMIT $3`, state, after, limit+1)
	if err != nil {
		return Page{}, err
	}
	defer rows.Close()
	var lastSeq int64
	for rows.Next() {
		var a Announcement
		var seq int64
		if err = rows.Scan(&a.ID, &a.Kind, &a.Title, &a.Body, &a.LinkURL, &a.Severity, &a.Dismissible, &a.AudienceKind, &a.AudienceValue,
			&a.StartsAt, &a.EndsAt, &a.CreatedBy, &a.CreatedAt, &a.UpdatedAt, &a.RetractedAt, &a.State, &a.Reads, &a.Dismissals, &seq); err != nil {
			return Page{}, err
		}
		if len(page.Announcements) == limit {
			page.NextAfter = fmt.Sprint(lastSeq)
			break
		}
		page.Announcements = append(page.Announcements, a)
		lastSeq = seq
	}
	return page, rows.Err()
}

// AudienceExists refuses a team, user or plan that does not exist (other audiences always exist); customer notices
// reuse it so a typo cannot silently reach nobody.
func AudienceExists(ctx context.Context, tx *db.Tx, kind, value string) error {
	return audienceExists(ctx, tx, kind, value)
}
