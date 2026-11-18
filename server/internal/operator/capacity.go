package operator

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/config"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

// NodeWarning is one thing an operator should look at about a node. Severity is "critical", "warning" or "info".
type NodeWarning struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
}

// NodeCapacity is one node's row on the capacity page: what it reports about itself and what it carried.
type NodeCapacity struct {
	ID                  string        `json:"id"`
	Kind                string        `json:"kind"` // edge, relay or server
	State               string        `json:"state,omitempty"`
	Region              string        `json:"region,omitempty"`
	Site                string        `json:"site,omitempty"`
	Version             string        `json:"version,omitempty"`
	CPUPercent          *float64      `json:"cpu_percent,omitempty"`
	CPUSustainedPercent *float64      `json:"cpu_sustained_percent,omitempty"`
	RSSBytes            *int64        `json:"rss_bytes,omitempty"`
	Connections         *int64        `json:"connections,omitempty"`
	ConnectionLimit     int64         `json:"connection_limit,omitempty"`
	BytesToday          *int64        `json:"bytes_today,omitempty"`
	BytesMonth          *int64        `json:"bytes_month,omitempty"`
	AllowanceUsed       *float64      `json:"allowance_used,omitempty"` // fraction of the monthly allowance
	LastHeartbeatAt     *time.Time    `json:"last_heartbeat_at,omitempty"`
	MetricsAt           *time.Time    `json:"metrics_at,omitempty"`
	Warnings            []NodeWarning `json:"warnings"`
}

type CapacityThresholds struct {
	MonthlyTrafficBytes int64   `json:"monthly_traffic_bytes"`
	TrafficWarnFraction float64 `json:"traffic_warn_fraction"`
	EdgeCPUWarnPercent  float64 `json:"edge_cpu_warn_percent"`
	RelayRSSWarnBytes   int64   `json:"relay_rss_warn_bytes"`
	CPUSustainedSeconds int64   `json:"cpu_sustained_seconds"`
}

type CapacityPage struct {
	ObservedAt time.Time          `json:"observed_at"`
	Today      string             `json:"today"`       // the UTC day "today" means
	MonthStart string             `json:"month_start"` // the first UTC day of the month "this month" means
	Thresholds CapacityThresholds `json:"thresholds"`
	Total      int64              `json:"total"` // every node the page could list
	Items      []NodeCapacity     `json:"items"`
	NextAfter  string             `json:"next_after,omitempty"`
}

// SetNodeCapacity sets the warning thresholds (one place: config.NodeCapacity).
func (s *Service) SetNodeCapacity(c config.NodeCapacity) { s.capacity = c }

// serverStaleAfter: the control plane records itself every 30 s; a gap this long means the loop stopped.
const serverStaleAfter = 2 * time.Minute

// cpuSustainedWindow mirrors controlplane.CPUSustainedWindow (the operator package does not import controlplane).
const cpuSustainedWindow = 5 * time.Minute

// capacityNodes is the set the capacity page lists: every edge and relay in the node registry that is not retired, and
// every control-plane instance that recorded itself.
const capacityNodes = ` FROM paperboat.control_tunnel_nodes n
 FULL JOIN paperboat.node_capacity c ON c.node_id=n.id
 WHERE ((n.id IS NOT NULL AND n.state<>'retired' AND (cardinality(n.roles)=0 OR n.roles && ARRAY['edge','relay']::text[])) OR (n.id IS NULL AND c.role='server'))`

// readCapacity pages those nodes by id (after the id `after`), with their latest report, today's and this month's
// bytes, and the warnings the thresholds produce.
func (s *Service) readCapacity(ctx context.Context, tx *db.Tx, after string, limit int) (CapacityPage, error) {
	now := time.Now().UTC()
	today, monthStart := now.Format("2006-01-02"), now.Format("2006-01")+"-01"
	page := CapacityPage{ObservedAt: now, Today: today, MonthStart: monthStart, Items: []NodeCapacity{},
		Thresholds: CapacityThresholds{s.capacity.MonthlyTrafficBytes, s.capacity.TrafficWarnFraction, s.capacity.EdgeCPUWarnPercent, s.capacity.RelayRSSWarnBytes, int64(cpuSustainedWindow.Seconds())}}
	if err := tx.QueryRow(ctx, `SELECT count(*)`+capacityNodes).Scan(&page.Total); err != nil {
		return page, err
	}
	// The day totals are summed for this page's nodes only (the (node_id, day) primary key serves the lookup).
	rows, err := tx.Query(ctx, `WITH p AS (SELECT coalesce(n.id,c.node_id) AS id`+capacityNodes+` AND coalesce(n.id,c.node_id)>$3 ORDER BY 1 LIMIT $4),
 d AS (SELECT node_id,
   sum(bytes_in+bytes_out) FILTER (WHERE day=$1::date) AS today, sum(bytes_in+bytes_out) AS month
  FROM paperboat.node_capacity_daily WHERE day>=$2::date AND node_id IN (SELECT id FROM p) GROUP BY node_id)
 SELECT coalesce(n.id,c.node_id), coalesce(n.roles,'{}'), coalesce(c.role,''), coalesce(n.state,''), coalesce(n.region,''), coalesce(n.site,''),
   coalesce(c.version,''), c.cpu_percent, c.cpu_sustained_percent, c.rss_bytes, coalesce(c.connections, n.capacity_used), coalesce(n.capacity_limit,0),
   coalesce(d.today,0), coalesce(d.month,0), n.last_heartbeat_at, c.reported_at, (c.node_id IS NOT NULL)
 FROM paperboat.control_tunnel_nodes n
 FULL JOIN paperboat.node_capacity c ON c.node_id=n.id
 LEFT JOIN d ON d.node_id=coalesce(n.id,c.node_id)
 WHERE coalesce(n.id,c.node_id) IN (SELECT id FROM p)
 ORDER BY coalesce(n.id,c.node_id)`, today, monthStart, after, limit+1)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		var v NodeCapacity
		var roles []string
		var role string
		var cpu, sustained sql.NullFloat64
		var rss, connections sql.NullInt64
		var today, month int64
		var heartbeat, reported sql.NullTime
		var reports bool
		if err := rows.Scan(&v.ID, &roles, &role, &v.State, &v.Region, &v.Site, &v.Version, &cpu, &sustained, &rss, &connections, &v.ConnectionLimit, &today, &month, &heartbeat, &reported, &reports); err != nil {
			return page, err
		}
		v.Kind = role
		if v.Kind == "" {
			v.Kind = "edge"
			for _, r := range roles {
				if r == "relay" {
					v.Kind = "relay"
				}
			}
		}
		if cpu.Valid {
			v.CPUPercent, v.CPUSustainedPercent = &cpu.Float64, &sustained.Float64
			v.RSSBytes = &rss.Int64
		}
		if connections.Valid {
			v.Connections = &connections.Int64
		}
		if reports {
			v.BytesToday, v.BytesMonth = &today, &month
			used := float64(month) / float64(s.capacity.MonthlyTrafficBytes)
			v.AllowanceUsed = &used
		}
		if heartbeat.Valid {
			v.LastHeartbeatAt = &heartbeat.Time
		}
		if reported.Valid {
			v.MetricsAt = &reported.Time
			if v.Kind == "server" {
				v.LastHeartbeatAt = &reported.Time
			}
		}
		v.Warnings = s.nodeWarnings(v, now, reports)
		page.Items = append(page.Items, v)
	}
	if len(page.Items) > limit {
		page.Items, page.NextAfter = page.Items[:limit], page.Items[limit-1].ID
	}
	return page, rows.Err()
}

// bytesText writes a byte count in decimal units (KB, MB, GB, TB), the way the panel and the traffic allowance are stated.
func bytesText(n float64) string {
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	i := 0
	for n >= 1000 && i < len(units)-1 {
		n /= 1000
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%.0f B", n)
	}
	return fmt.Sprintf("%.1f %s", n, units[i])
}
func gib(n float64) string { return fmt.Sprintf("%.1f GiB", n/(1<<30)) }

// nodeWarnings applies the thresholds. "Sustained" CPU is the server's weighted five minute average, not one sample.
func (s *Service) nodeWarnings(v NodeCapacity, now time.Time, reports bool) []NodeWarning {
	out := []NodeWarning{}
	stale := s.nodeStaleAfter
	if v.Kind == "server" {
		stale = serverStaleAfter
	}
	switch {
	case v.LastHeartbeatAt == nil:
		out = append(out, NodeWarning{"no_heartbeat", "critical", "This node has never sent a heartbeat."})
	case now.Sub(*v.LastHeartbeatAt) > stale:
		out = append(out, NodeWarning{"heartbeat_stale", "critical", fmt.Sprintf("No heartbeat for %d minutes.", int(now.Sub(*v.LastHeartbeatAt).Minutes()))})
	}
	if !reports {
		return append(out, NodeWarning{"no_metrics", "info", "This node does not report CPU, memory or traffic yet (an older build)."})
	}
	if v.Kind == "edge" && v.CPUSustainedPercent != nil && float64(*v.CPUSustainedPercent) >= s.capacity.EdgeCPUWarnPercent {
		out = append(out, NodeWarning{"edge_cpu_high", "warning", fmt.Sprintf("Sustained CPU is %.0f%% (warning at %.0f%%).", *v.CPUSustainedPercent, s.capacity.EdgeCPUWarnPercent)})
	}
	if v.Kind == "relay" && v.RSSBytes != nil && *v.RSSBytes >= s.capacity.RelayRSSWarnBytes {
		out = append(out, NodeWarning{"relay_memory_high", "warning", fmt.Sprintf("Memory is %s (warning at %s).", gib(float64(*v.RSSBytes)), gib(float64(s.capacity.RelayRSSWarnBytes)))})
	}
	if v.BytesMonth != nil && v.Kind != "server" {
		allowance := float64(s.capacity.MonthlyTrafficBytes)
		switch used := float64(*v.BytesMonth); {
		case used >= allowance:
			out = append(out, NodeWarning{"traffic_allowance_exceeded", "critical", fmt.Sprintf("This month's traffic %s is past the %s allowance.", bytesText(used), bytesText(allowance))})
		case used >= allowance*s.capacity.TrafficWarnFraction:
			out = append(out, NodeWarning{"traffic_allowance_high", "warning", fmt.Sprintf("This month's traffic %s is %.0f%% of the %s allowance.", bytesText(used), used/allowance*100, bytesText(allowance))})
		}
	}
	return out
}
