package controlplane

import (
	"context"
	"database/sql"
	"errors"

	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

// ConfigRepositoryItem is a listed repository with how many machines use it (live assignments that pull
// from or push to it) and whether it is the selected team workspace's config default.
type ConfigRepositoryItem struct {
	dbsqlc.ControlConfigRepository
	MachineCount int
	TeamDefault  bool
}

func (s *ConfigAssignmentService) Repository(ctx context.Context, account, id string) (dbsqlc.ControlConfigRepository, error) {
	item, err := s.store.Queries().GetOwnedControlConfigRepository(ctx, dbsqlc.GetOwnedControlConfigRepositoryParams{ID: id, OwnerUserID: account})
	if errors.Is(err, sql.ErrNoRows) {
		return item, ErrAssignmentForbidden
	}
	return item, err
}
func (s *ConfigAssignmentService) RepositoryInventory(ctx context.Context, account, q, state, owner string, limit, offset int) ([]ConfigRepositoryItem, int, error) {
	if limit < 1 || limit > 200 || offset < 0 {
		return nil, 0, ErrAssignmentInvalid
	}
	predicate := ` FROM control_config_repositories WHERE owner_user_id=$1 AND state=CASE WHEN $3='' THEN 'active' ELSE $3 END
 AND ($2='' OR position(lower($2) in lower(display_name||' '||id||' '||provider))>0)
 AND ($4='' OR $4='mine' OR owner_user_id=$4)`
	args := []any{account, q, state, owner}
	var total int
	if err := s.store.SQL().QueryRowContext(ctx, "SELECT count(*)"+predicate, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	team := ""
	if scope, ok := workspace.FromContext(ctx); ok {
		team = scope.TeamID
	}
	// A team default names a repository by provider and external id (a custom Git repository's URL), the way
	// SetTeamDefault stores it and AdoptTeamConfigDefault matches it.
	rows, err := s.store.SQL().QueryContext(ctx, `SELECT id,
 (SELECT count(DISTINCT a.machine_id) FROM control_config_assignments a JOIN live_user_machines m ON m.id=a.machine_id
  WHERE a.revoked_at IS NULL AND (a.repository_id=control_config_repositories.id OR a.push_repository_id=control_config_repositories.id))::int,
 EXISTS(SELECT 1 FROM team_config_defaults d WHERE d.team_id=$7 AND d.provider=control_config_repositories.provider
  AND d.external_repository_id=coalesce(control_config_repositories.external_repository_id,control_config_repositories.external_ref))`+
		predicate+" ORDER BY lower(display_name),id LIMIT $5 OFFSET $6", append(args, limit, offset, team)...)
	if err != nil {
		return nil, 0, err
	}
	ids := []string{}
	extras := map[string]ConfigRepositoryItem{}
	for rows.Next() {
		var id string
		var extra ConfigRepositoryItem
		if err = rows.Scan(&id, &extra.MachineCount, &extra.TeamDefault); err != nil {
			rows.Close()
			return nil, 0, err
		}
		ids = append(ids, id)
		extras[id] = extra
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, 0, err
	}
	out := make([]ConfigRepositoryItem, 0, len(ids))
	for _, id := range ids {
		item, e := s.store.Queries().GetActiveControlConfigRepository(ctx, id)
		if e != nil {
			return nil, 0, e
		}
		if item.OwnerUserID != account {
			return nil, 0, ErrAssignmentForbidden
		}
		extra := extras[id]
		extra.ControlConfigRepository = item
		out = append(out, extra)
	}
	return out, total, nil
}
