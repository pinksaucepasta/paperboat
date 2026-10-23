package inspectaccess

import (
	"context"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

type Target struct {
	ResourceID string `json:"resource_id"`
	RouteID    string `json:"route_id"`
	RouteName  string `json:"route_name"`
}

// Targets exposes bounded routing metadata only to principals holding the exact
// requested inspector action. It does not grant application use or management.
func (s *Service) Targets(ctx context.Context, account, selector, action string) ([]Target, error) {
	if !validID(account) || !validID(selector) || !validAction(action) {
		return nil, ErrInvalid
	}
	result := []Target{}
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		rows, err := tx.Query(ctx, `SELECT t.id,r.id,r.name FROM tunnels t JOIN tunnel_routes r ON r.tunnel_id=t.id WHERE (t.id=$1 OR t.name=$1) AND t.desired_state='active' AND t.deleted_at IS NULL AND (t.expires_at IS NULL OR t.expires_at>$3) AND r.desired_state='active' AND r.deleted_at IS NULL AND (t.account_id=$2 OR EXISTS(SELECT 1 FROM team_resource_bindings b WHERE b.resource_kind='tunnel' AND b.resource_id=t.id AND b.active AND workspace_policy_resource_allowed($2,b.team_id,'tunnel',t.id,$4))) ORDER BY t.id,r.id LIMIT 129`, selector, account, s.now(), action)
		if err != nil {
			return err
		}
		candidates := []Target{}
		for rows.Next() {
			var candidate Target
			if err = rows.Scan(&candidate.ResourceID, &candidate.RouteID, &candidate.RouteName); err != nil {
				rows.Close()
				return err
			}
			candidates = append(candidates, candidate)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(candidates) > 128 {
			return ErrCapacity
		}
		for _, candidate := range candidates {
			a, e := resolveResource(ctx, tx, "tunnel", candidate.ResourceID, candidate.RouteID, s.now())
			if e != nil {
				return e
			}
			if _, e = resolvePrincipal(ctx, tx, a, account, action); e != nil {
				if normalizeAuthorizationError(e) == ErrNotAuthorized {
					continue
				}
				return e
			}
			result = append(result, candidate)
		}
		if len(result) == 0 {
			return ErrNotAuthorized
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
