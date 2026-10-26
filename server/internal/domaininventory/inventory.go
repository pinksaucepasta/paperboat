// Package domaininventory lists the custom domains of a workspace in one paged list (the console's Domains page,
// Lane C PQ-04 C2): the hostnames of the tunnels the caller may manage and of its live previews, in hostname
// order, with an exact total. Authorization is the tunnel and preview lists' own (manageable_tunnel_ids,
// manageable_preview_ids), so the page shows exactly the domains of what those lists show.
package domaininventory

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/previewdomain"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelv1"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

var ErrInvalidInput = errors.New("domain inventory request is invalid")

// Position is a keyset position: the lowercased hostname and the domain id.
type Position struct{ Key, ID string }

type Options struct {
	Limit int
	After *Position
	Q     string // hostname substring
	Kind  string // "", "tunnel" or "preview"
}

// Item is one domain: kind "tunnel" with a tunnel domain binding, or "preview" with a preview custom domain.
type Item struct {
	Kind      string `json:"kind"`
	Domain    any    `json:"domain"`
	OwnerName string `json:"owner_name,omitempty"` // kind tunnel: the tunnel's name
	Endpoint  string `json:"endpoint,omitempty"`   // kind preview: the preview's endpoint URL
	key       string
	id        string
}

type Page struct {
	Items []Item
	Total int
	More  bool
}

// Next is the position after the page's last item.
func (p Page) Next() Position {
	last := p.Items[len(p.Items)-1]
	return Position{Key: last.key, ID: last.id}
}

type Service struct{ db *db.DB }

func New(store *db.DB) *Service { return &Service{db: store} }

func (s *Service) List(ctx context.Context, account string, opts Options) (Page, error) {
	if strings.TrimSpace(account) == "" || opts.Limit < 1 || opts.Limit > 200 || (opts.Kind != "" && opts.Kind != "tunnel" && opts.Kind != "preview") {
		return Page{}, ErrInvalidInput
	}
	ws, selected := workspace.FromContext(ctx)
	if !selected {
		ws, selected = workspace.Scope{ID: "personal"}, true
	}
	if err := workspace.Validate(ctx, s.db, account, ws); err != nil {
		return Page{}, err
	}
	var team, afterKey, afterID any
	if ws.TeamID != "" {
		team = ws.TeamID
	}
	if opts.After != nil {
		afterKey, afterID = opts.After.Key, opts.After.ID
	}
	// The window counts the whole filtered set before the cursor narrows it: the total is the same on every page.
	rows, err := s.db.SQL().QueryContext(ctx, `WITH tunnels AS (
 SELECT paperboat.manageable_tunnel_ids($1, $2, $3, '', '', '', '', NULL, NULL, NULL) AS id
), previews AS (
 SELECT paperboat.manageable_preview_ids($1, $2, $3, '', 'active', '', '', NULL, NULL, NULL) AS id
), domains AS (
 SELECT 'tunnel'::text AS kind, d.id, lower(d.hostname) AS k FROM paperboat.tunnel_domains d
 WHERE $4 IN ('', 'tunnel') AND d.deleted_at IS NULL AND d.tunnel_id IN (SELECT id FROM tunnels)
 UNION ALL
 SELECT 'preview', d.id, lower(d.hostname) FROM paperboat.preview_domains d
 WHERE $4 IN ('', 'preview') AND d.deleted_at IS NULL AND d.preview_id IN (SELECT id FROM previews)
)
SELECT x.kind, x.id, x.k, x.total FROM (
 SELECT d.*, count(*) OVER () AS total FROM domains d WHERE $5 = '' OR position(lower($5) in d.k) > 0
) x WHERE $6::text IS NULL OR (x.k, x.id) > ($6::text, $7::text)
ORDER BY x.k, x.id LIMIT $8`, account, selected, team, opts.Kind, opts.Q, afterKey, afterID, opts.Limit+1)
	if err != nil {
		return Page{}, err
	}
	page := Page{Total: -1}
	var tunnelIDs, previewIDs []string
	for rows.Next() {
		var item Item
		if err := rows.Scan(&item.Kind, &item.id, &item.key, &page.Total); err != nil {
			return Page{}, errors.Join(err, rows.Close())
		}
		page.Items = append(page.Items, item)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return Page{}, err
	}
	if len(page.Items) > opts.Limit {
		page.Items, page.More = page.Items[:opts.Limit], true
	}
	if page.Total < 0 {
		if page.Total, err = s.count(ctx, account, selected, team, opts); err != nil {
			return Page{}, err
		}
	}
	for _, item := range page.Items {
		if item.Kind == "tunnel" {
			tunnelIDs = append(tunnelIDs, item.id)
		} else {
			previewIDs = append(previewIDs, item.id)
		}
	}
	views, owners := map[string]any{}, map[string]string{}
	if len(tunnelIDs) > 0 {
		found, err := s.db.Queries().ListTunnelDomainsByIDsV1(ctx, tunnelIDs)
		if err != nil {
			return Page{}, err
		}
		for _, row := range found {
			views["tunnel/"+row.ID] = tunnelv1.DomainViewFromRow(row)
			owners["tunnel/"+row.ID] = row.TunnelID
		}
	}
	if len(previewIDs) > 0 {
		found, err := s.db.Queries().ListPreviewDomainsByIDsV1(ctx, previewIDs)
		if err != nil {
			return Page{}, err
		}
		for _, row := range found {
			views["preview/"+row.ID] = previewdomain.DomainViewFromRow(row)
			owners["preview/"+row.ID] = row.PreviewID
		}
	}
	// The owners' names (tunnels) and addresses (previews) for the page, so a row links without a lookup.
	var tunnels, previews []string
	for _, item := range page.Items {
		if item.Kind == "tunnel" {
			tunnels = append(tunnels, owners["tunnel/"+item.id])
		} else {
			previews = append(previews, owners["preview/"+item.id])
		}
	}
	labels := map[string]string{}
	rows, err = s.db.SQL().QueryContext(ctx, `SELECT 'tunnel/'||id,name FROM paperboat.tunnels WHERE id=ANY($1)
 UNION ALL SELECT 'preview/'||id,endpoint FROM paperboat.preview_leases WHERE id=ANY($2)`, tunnels, previews)
	if err != nil {
		return Page{}, err
	}
	for rows.Next() {
		var key, label string
		if err := rows.Scan(&key, &label); err != nil {
			return Page{}, errors.Join(err, rows.Close())
		}
		labels[key] = label
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return Page{}, err
	}
	for i := range page.Items {
		key := page.Items[i].Kind + "/" + page.Items[i].id
		view, found := views[key]
		if !found {
			return Page{}, sql.ErrNoRows // removed between the two reads; the caller retries
		}
		page.Items[i].Domain = view
		if page.Items[i].Kind == "tunnel" {
			page.Items[i].OwnerName = labels["tunnel/"+owners[key]]
		} else {
			page.Items[i].Endpoint = labels["preview/"+owners[key]]
		}
	}
	if page.Items == nil {
		page.Items = []Item{}
	}
	return page, nil
}

// count is the total for a page past the end (the window had no row to carry it).
func (s *Service) count(ctx context.Context, account string, selected bool, team any, opts Options) (int, error) {
	var total int
	err := s.db.SQL().QueryRowContext(ctx, `SELECT
 (SELECT count(*) FROM paperboat.tunnel_domains d WHERE $4 IN ('', 'tunnel') AND d.deleted_at IS NULL AND ($5 = '' OR position(lower($5) in lower(d.hostname)) > 0)
   AND d.tunnel_id IN (SELECT paperboat.manageable_tunnel_ids($1, $2, $3, '', '', '', '', NULL, NULL, NULL)))
 + (SELECT count(*) FROM paperboat.preview_domains d WHERE $4 IN ('', 'preview') AND d.deleted_at IS NULL AND ($5 = '' OR position(lower($5) in lower(d.hostname)) > 0)
   AND d.preview_id IN (SELECT paperboat.manageable_preview_ids($1, $2, $3, '', 'active', '', '', NULL, NULL, NULL)))`,
		account, selected, team, opts.Kind, opts.Q).Scan(&total)
	return total, err
}
