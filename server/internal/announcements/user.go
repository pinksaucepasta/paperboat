package announcements

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// MaxNotices is how many notices the bell holds; MaxMarked bounds one mark-as-read request.
const (
	MaxNotices = 20
	MaxMarked  = 50
)

// Banner is the ribbon a user sees.
type Banner struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	LinkURL     string `json:"link_url,omitempty"`
	Severity    string `json:"severity"`
	Dismissible bool   `json:"dismissible"`
}

// Notice is one row of the bell.
type Notice struct {
	ID      string    `json:"id"`
	Title   string    `json:"title"`
	Body    string    `json:"body"`
	LinkURL string    `json:"link_url,omitempty"`
	SentAt  time.Time `json:"sent_at"`
	Read    bool      `json:"read"`
}

// View is everything one user sees of announcements: nothing at all is an empty list and no banner.
type View struct {
	Banner  *Banner  `json:"banner"`
	Notices []Notice `json:"notices"`
	Unread  int      `json:"unread"`
}

// Audience matches announcement a to user $1: everyone, the user, a member of the team, or someone on the plan
// (their own paid plan or one of their teams'; free is the audience without a paid personal plan).
const (
	paidStates = `b.state IN ('active','past_due')`
	audience   = `(a.audience_kind='all'
 OR (a.audience_kind='user' AND a.audience_value=$1)
 OR (a.audience_kind='team' AND EXISTS(SELECT 1 FROM paperboat.team_members m WHERE m.team_id=a.audience_value AND m.account_id=$1 AND m.active))
 OR (a.audience_kind='plan' AND a.audience_value='free' AND NOT EXISTS(SELECT 1 FROM paperboat.billing_workspaces b WHERE b.user_id=$1 AND b.product_code IS NOT NULL AND ` + paidStates + `))
 OR (a.audience_kind='plan' AND EXISTS(SELECT 1 FROM paperboat.billing_workspaces b WHERE b.product_code=a.audience_value AND ` + paidStates + `
   AND (b.user_id=$1 OR b.team_id IN (SELECT m.team_id FROM paperboat.team_members m WHERE m.account_id=$1 AND m.active)))))`
	live = `a.retracted_at IS NULL AND a.starts_at<=now() AND (a.ends_at IS NULL OR a.ends_at>now())`
)

// ForUser is the user's banner (the most specific audience, then the most severe) and bell.
func (s *Service) ForUser(ctx context.Context, userID string) (View, error) {
	view := View{Notices: []Notice{}}
	var b Banner
	err := s.store.Pool().QueryRow(ctx, `SELECT a.id,a.title,a.body,coalesce(a.link_url,''),a.severity,a.dismissible FROM paperboat.announcements a
 LEFT JOIN paperboat.announcement_receipts r ON r.announcement_id=a.id AND r.user_id=$1
 WHERE a.kind='banner' AND `+live+` AND `+audience+` AND (r.dismissed_at IS NULL OR NOT a.dismissible)
 ORDER BY CASE a.audience_kind WHEN 'user' THEN 0 WHEN 'team' THEN 1 WHEN 'plan' THEN 2 ELSE 3 END,
  CASE a.severity WHEN 'critical' THEN 0 WHEN 'warning' THEN 1 ELSE 2 END, a.starts_at DESC, a.seq DESC LIMIT 1`, userID).
		Scan(&b.ID, &b.Title, &b.Body, &b.LinkURL, &b.Severity, &b.Dismissible)
	switch {
	case err == nil:
		view.Banner = &b
	case !errors.Is(err, pgx.ErrNoRows):
		return View{}, err
	}
	rows, err := s.store.Pool().Query(ctx, `SELECT a.id,a.title,a.body,coalesce(a.link_url,''),a.starts_at,r.read_at IS NOT NULL FROM paperboat.announcements a
 LEFT JOIN paperboat.announcement_receipts r ON r.announcement_id=a.id AND r.user_id=$1
 WHERE a.kind='notice' AND `+live+` AND `+audience+` ORDER BY a.starts_at DESC, a.seq DESC LIMIT $2`, userID, MaxNotices)
	if err != nil {
		return View{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var n Notice
		if err = rows.Scan(&n.ID, &n.Title, &n.Body, &n.LinkURL, &n.SentAt, &n.Read); err != nil {
			return View{}, err
		}
		if !n.Read {
			view.Unread++
		}
		view.Notices = append(view.Notices, n)
	}
	return view, rows.Err()
}

// MarkRead marks the notices read for the user. An id that is not a notice they can see is ignored: nobody can plant receipts.
func (s *Service) MarkRead(ctx context.Context, userID string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if len(ids) > MaxMarked {
		return ErrInvalid
	}
	_, err := s.store.Pool().Exec(ctx, `INSERT INTO paperboat.announcement_receipts(announcement_id,user_id,read_at)
 SELECT a.id,$1,now() FROM paperboat.announcements a WHERE a.id=ANY($2) AND a.kind='notice' AND `+live+` AND `+audience+`
 ON CONFLICT (announcement_id,user_id) DO UPDATE SET read_at=coalesce(announcement_receipts.read_at,now())`, userID, ids)
	return err
}

// Dismiss hides a dismissible banner from the user for good.
func (s *Service) Dismiss(ctx context.Context, userID, id string) error {
	tag, err := s.store.Pool().Exec(ctx, `INSERT INTO paperboat.announcement_receipts(announcement_id,user_id,dismissed_at)
 SELECT a.id,$1,now() FROM paperboat.announcements a WHERE a.id=$2 AND a.kind='banner' AND a.dismissible AND `+live+` AND `+audience+`
 ON CONFLICT (announcement_id,user_id) DO UPDATE SET dismissed_at=coalesce(announcement_receipts.dismissed_at,now())`, userID, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
