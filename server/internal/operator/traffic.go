package operator

import (
	"context"
	"fmt"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// TrafficCategories are the five ways bytes travel, in the order the dashboard lists them.
var TrafficCategories = []string{"direct", "relay_managed", "relay_self_hosted", "edge_managed", "edge_self_hosted"}

// TrafficTotal is one category's bytes in the period. Upload is bytes sent towards the carrier (a node's bytes in),
// download the bytes it sent back. Reporting of Expected sources sent a figure: machines active in the period that report
// usage (direct), or nodes that sent a sample (relays and edges). CoveragePercent is nil when nothing is expected.
type TrafficTotal struct {
	Category        string   `json:"category"`
	UploadBytes     int64    `json:"upload_bytes"`
	DownloadBytes   int64    `json:"download_bytes"`
	Reporting       int64    `json:"reporting"`
	Expected        int64    `json:"expected"`
	CoveragePercent *float64 `json:"coverage_percent"`
}

type TrafficPoint struct {
	At            time.Time `json:"at"`
	Category      string    `json:"category"`
	UploadBytes   int64     `json:"upload_bytes"`
	DownloadBytes int64     `json:"download_bytes"`
}

type TrafficPage struct {
	Period string         `json:"period"`
	From   time.Time      `json:"from"`
	To     time.Time      `json:"to"`
	Grain  string         `json:"grain"`
	Totals []TrafficTotal `json:"totals"`
	Series []TrafficPoint `json:"series"`
}

type TrafficNode struct {
	NodeID        string `json:"node_id"`
	Region        string `json:"region"`
	State         string `json:"state"`
	Reporting     bool   `json:"reporting"`
	UploadBytes   int64  `json:"upload_bytes"`
	DownloadBytes int64  `json:"download_bytes"`
}

type TrafficNodePage struct {
	Period    string        `json:"period"`
	Kind      string        `json:"kind"`
	Items     []TrafficNode `json:"items"`
	NextAfter string        `json:"next_after,omitempty"`
}

// trafficPeriod resolves a period name to its start (UTC) and bucket size. "month" is the calendar month in UTC.
func trafficPeriod(name string, now time.Time) (from time.Time, grain string, err error) {
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	switch name {
	case "today":
		return today, "hour", nil
	case "7d":
		return today.AddDate(0, 0, -6), "day", nil
	case "30d":
		return today.AddDate(0, 0, -29), "day", nil
	case "month":
		return today.AddDate(0, 0, 1-today.Day()), "day", nil
	}
	return from, "", ErrInvalid
}

// trafficRows is the one place the hour and day tables meet: rows of (at, category, node_id, u, d) bucketed by $2
// ('hour' or 'day'). Hours past their window were moved into days atomically, so no byte is in both.
const trafficRows = `r AS (
 SELECT CASE WHEN $2='hour' THEN hour ELSE date_trunc('day',hour AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' END AS at,category,node_id,upload_bytes AS u,download_bytes AS d
 FROM paperboat.traffic_hourly WHERE hour>=$1
 UNION ALL SELECT day::timestamp AT TIME ZONE 'UTC',category,node_id,upload_bytes,download_bytes FROM paperboat.traffic_daily WHERE $2='day' AND day>=($1 AT TIME ZONE 'UTC')::date)`

// selfHostedNode is the rule project_edge_bandwidth uses: a node installed by an account or team is theirs.
const selfHostedNode = `EXISTS(SELECT 1 FROM paperboat.selfhost_installations i WHERE i.node_id=%s AND i.scope_kind IN ('account','team'))`

func (s *Service) readTraffic(ctx context.Context, tx *db.Tx, period string) (TrafficPage, error) {
	now := time.Now().UTC()
	from, grain, err := trafficPeriod(period, now)
	if err != nil {
		return TrafficPage{}, err
	}
	page := TrafficPage{Period: period, From: from, To: now, Grain: grain, Totals: make([]TrafficTotal, len(TrafficCategories)), Series: []TrafficPoint{}}
	byCategory := map[string]*TrafficTotal{}
	for i, c := range TrafficCategories {
		page.Totals[i].Category = c
		byCategory[c] = &page.Totals[i]
	}
	rows, err := tx.Query(ctx, `WITH `+trafficRows+` SELECT at,category,sum(u)::bigint,sum(d)::bigint FROM r GROUP BY 1,2 ORDER BY 1,2`, from, grain)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		var p TrafficPoint
		if err := rows.Scan(&p.At, &p.Category, &p.UploadBytes, &p.DownloadBytes); err != nil {
			return page, err
		}
		t := byCategory[p.Category]
		t.UploadBytes += p.UploadBytes
		t.DownloadBytes += p.DownloadBytes
		page.Series = append(page.Series, p)
	}
	if err := rows.Err(); err != nil {
		return page, err
	}
	rows.Close()
	// Coverage: machines active in the period that have ever sent a usage report, and per kind of node the nodes
	// that sent a sample against the nodes registered (ours) or installed (customers').
	cover, err := tx.Query(ctx, `WITH `+trafficRows+`,
 ids AS (SELECT DISTINCT category,node_id FROM r WHERE category<>'direct'),
 nodes AS (
  SELECT CASE WHEN 'relay'=ANY(n.roles) THEN 'relay_managed' ELSE 'edge_managed' END AS category,n.id FROM paperboat.control_tunnel_nodes n
   WHERE NOT `+sprintfSelfHosted("n.id")+`
  UNION ALL SELECT CASE i.capability WHEN 'relay' THEN 'relay_self_hosted' ELSE 'edge_self_hosted' END,i.node_id FROM paperboat.selfhost_installations i
   WHERE i.state='active' AND i.scope_kind IN ('account','team'))
 SELECT category,(SELECT count(*) FROM ids WHERE ids.category=c.category AND ids.node_id IN (SELECT id FROM nodes WHERE nodes.category=c.category)),
  (SELECT count(*) FROM nodes WHERE nodes.category=c.category)
 FROM unnest($3::text[]) AS c(category) WHERE category<>'direct'
 UNION ALL SELECT 'direct',count(*) FILTER (WHERE EXISTS(SELECT 1 FROM paperboat.native_usage_reporting_status s WHERE s.machine_id=m.id)),count(*)
 FROM paperboat.user_machines m WHERE m.deleted_at IS NULL AND m.last_seen_at>=$1`, from, grain, TrafficCategories)
	if err != nil {
		return page, err
	}
	defer cover.Close()
	for cover.Next() {
		var category string
		var reporting, expected int64
		if err := cover.Scan(&category, &reporting, &expected); err != nil {
			return page, err
		}
		t := byCategory[category]
		t.Reporting, t.Expected = reporting, expected
		if expected > 0 {
			pct := float64(min(reporting, expected)) * 100 / float64(expected)
			t.CoveragePercent = &pct
		}
	}
	return page, cover.Err()
}

// readTrafficNodes pages one kind of our own ("relay" or "edge") nodes by id with their bytes in the period. A
// registered node that sent nothing is listed with zero bytes and reporting=false: a silent node is the finding.
func (s *Service) readTrafficNodes(ctx context.Context, tx *db.Tx, period, kind, after string, limit int) (TrafficNodePage, error) {
	from, grain, err := trafficPeriod(period, time.Now().UTC())
	if err != nil || (kind != "relay" && kind != "edge") {
		return TrafficNodePage{}, ErrInvalid
	}
	page := TrafficNodePage{Period: period, Kind: kind, Items: []TrafficNode{}}
	rows, err := tx.Query(ctx, `WITH `+trafficRows+`,
 sums AS (SELECT node_id,sum(u)::bigint u,sum(d)::bigint d FROM r WHERE category=$3||'_managed' GROUP BY 1),
 ids AS (SELECT node_id AS id FROM sums UNION SELECT n.id FROM paperboat.control_tunnel_nodes n
  WHERE ('relay'=ANY(n.roles))=($3='relay') AND NOT `+sprintfSelfHosted("n.id")+`),
 p AS (SELECT id FROM ids WHERE id>$4 ORDER BY id LIMIT $5)
 SELECT p.id,coalesce(n.region,''),coalesce(n.state,''),s.node_id IS NOT NULL,coalesce(s.u,0),coalesce(s.d,0)
 FROM p LEFT JOIN paperboat.control_tunnel_nodes n ON n.id=p.id LEFT JOIN sums s ON s.node_id=p.id ORDER BY p.id`, from, grain, kind, after, limit+1)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		var v TrafficNode
		if err := rows.Scan(&v.NodeID, &v.Region, &v.State, &v.Reporting, &v.UploadBytes, &v.DownloadBytes); err != nil {
			return page, err
		}
		page.Items = append(page.Items, v)
	}
	if len(page.Items) > limit {
		page.Items, page.NextAfter = page.Items[:limit], page.Items[limit-1].NodeID
	}
	return page, rows.Err()
}

func sprintfSelfHosted(column string) string { return fmt.Sprintf(selfHostedNode, column) }
