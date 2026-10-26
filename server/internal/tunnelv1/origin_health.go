package tunnelv1

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
)

// OriginHealthMaxAge is how long an unreachable report keeps the tunnel's origin down. The host
// repeats an unreachable state every 30 seconds, so a missing refresh ages the state out.
const OriginHealthMaxAge = 60 * time.Second

// OriginRouteReport is one route's origin state as reported by the machine that serves the tunnel.
type OriginRouteReport struct {
	TunnelID   string
	RouteID    string
	State      string // "ready" or "unreachable"
	Code       string // refused, timeout, unreachable or tls when unreachable
	ObservedAt time.Time
}

// RecordOriginHealth stores the latest origin state per route reported by the machine and writes a
// tunnel log line when a route changes between reachable and unreachable. Routes that are not
// active or whose tunnel the machine does not serve are ignored:
// the report is a request, not authority.
func (r *SQLRepository) RecordOriginHealth(ctx context.Context, accountID, machineID, correlationID string, routes []OriginRouteReport, now time.Time) error {
	if accountID == "" || machineID == "" || len(routes) == 0 {
		return ErrInvalidInput
	}
	return r.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		for _, route := range routes {
			var active bool
			err := tx.QueryRow(ctx, `SELECT true FROM tunnel_routes r JOIN tunnels t ON t.id=r.tunnel_id AND t.account_id=$1
JOIN endpoints e ON e.tunnel_id=t.id AND e.kind='tunnel' AND e.agent_machine_id=$2 AND e.desired_state='active'
WHERE r.id=$3 AND r.tunnel_id=$4 AND r.desired_state='active' AND r.deleted_at IS NULL`,
				accountID, machineID, route.RouteID, route.TunnelID).Scan(&active)
			if errors.Is(err, pgx.ErrNoRows) {
				continue
			}
			if err != nil {
				return fmt.Errorf("read route for origin health: %w", err)
			}
			var previous sql.NullString
			err = tx.QueryRow(ctx, `SELECT state FROM tunnel_route_origin_health WHERE route_id=$1 AND machine_id=$2 FOR UPDATE`, route.RouteID, machineID).Scan(&previous)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("read previous origin health: %w", err)
			}
			code := sql.NullString{String: route.Code, Valid: route.State == "unreachable"}
			if _, err := tx.Exec(ctx, `INSERT INTO tunnel_route_origin_health(route_id,machine_id,tunnel_id,state,code,observed_at,reported_at) VALUES($1,$2,$3,$4,$5,$6,$7)
ON CONFLICT (route_id,machine_id) DO UPDATE SET state=EXCLUDED.state,code=EXCLUDED.code,observed_at=EXCLUDED.observed_at,reported_at=EXCLUDED.reported_at
WHERE tunnel_route_origin_health.observed_at<=EXCLUDED.observed_at`, route.RouteID, machineID, route.TunnelID, route.State, code, route.ObservedAt.UTC(), now.UTC()); err != nil {
				return fmt.Errorf("store origin health: %w", err)
			}
			wasUnreachable := previous.Valid && previous.String == "unreachable"
			isUnreachable := route.State == "unreachable"
			if wasUnreachable == isUnreachable || !previous.Valid && !isUnreachable {
				continue
			}
			level, logCode, message := "warn", "tunnel_origin_unreachable", "The local app for this route is not reachable ("+route.Code+")."
			if !isUnreachable {
				level, logCode, message = "info", "tunnel_origin_recovered", "The local app for this route is reachable again."
			}
			digest := sha256.Sum256([]byte(route.RouteID + "\x00" + machineID + "\x00" + route.State + "\x00" + route.ObservedAt.UTC().Format(time.RFC3339Nano)))
			id := "log_origin_" + hex.EncodeToString(digest[:12])
			if _, err := tx.Queries().CreateTunnelLogEntryV1(ctx, dbsqlc.CreateTunnelLogEntryV1Params{
				ID: id, AccountID: accountID, TunnelID: nullableString(route.TunnelID), RouteID: nullableString(route.RouteID),
				Level: level, Component: "agent", Code: logCode, Message: message, Metadata: []byte(`{}`), CorrelationID: correlationID, OccurredAt: route.ObservedAt.UTC(),
			}); err != nil {
				return translate(err)
			}
		}
		return nil
	})
}
