package usermachines

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
)

// defaultRouteReconnectGrace is how long a descriptor request waits for a machine's runtime route that has just
// gone: a pb update replaces the host's connector, and the route is not "applied" for the new generation until the
// edge has taken it (pbvm4: sends and execs got machine_offline for the length of that handover while the machine
// stayed online). Headscale waits the same 10 s before it calls a node that dropped its map poll offline
// (~/ref/headscale/hscontrol/poll.go:172-200, "wait up to 10 seconds for the node to reconnect").
const defaultRouteReconnectGrace = 10 * time.Second

const routeReconnectInterval = 150 * time.Millisecond

// activeHelperRoute is GetActiveHelperRouteForEnvironment for a request that issues a descriptor: a missing route of a
// machine that reports online is waited for, up to the grace, before it is reported missing.
func (s *Service) activeHelperRoute(ctx context.Context, environmentID string) (dbsqlc.ControlRoute, error) {
	grace := s.policy.RouteReconnectGrace
	if grace == 0 {
		grace = defaultRouteReconnectGrace
	}
	return waitWhileNoRows(ctx, grace, routeReconnectInterval, func() (dbsqlc.ControlRoute, error) {
		return s.db.Queries().GetActiveHelperRouteForEnvironment(ctx, environmentID)
	})
}

// waitWhileNoRows repeats lookup while it finds nothing, until the grace ends or ctx is done; any other error and any
// result return at once.
func waitWhileNoRows[T any](ctx context.Context, grace, interval time.Duration, lookup func() (T, error)) (T, error) {
	deadline := time.Now().Add(grace)
	for {
		value, err := lookup()
		if !errors.Is(err, sql.ErrNoRows) || !time.Now().Add(interval).Before(deadline) {
			return value, err
		}
		timer := time.NewTimer(interval)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return value, err
		}
	}
}
