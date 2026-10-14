package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"github.com/pinksaucepasta/paperboat-server/internal/db/dbsqlc"
)

func (s *ConfigAssignmentService) Repository(ctx context.Context, account, id string) (dbsqlc.ControlConfigRepository, error) {
	item, err := s.store.Queries().GetOwnedControlConfigRepository(ctx, dbsqlc.GetOwnedControlConfigRepositoryParams{ID: id, OwnerUserID: account})
	if errors.Is(err, sql.ErrNoRows) {
		return item, ErrAssignmentForbidden
	}
	return item, err
}
func (s *ConfigAssignmentService) RepositoryInventory(ctx context.Context, account, q, state, owner string, limit, offset int) ([]dbsqlc.ControlConfigRepository, int, error) {
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
	rows, err := s.store.SQL().QueryContext(ctx, "SELECT id"+predicate+" ORDER BY lower(display_name),id LIMIT $5 OFFSET $6", append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, 0, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, 0, err
	}
	out := make([]dbsqlc.ControlConfigRepository, 0, len(ids))
	for _, id := range ids {
		item, e := s.store.Queries().GetActiveControlConfigRepository(ctx, id)
		if e != nil {
			return nil, 0, e
		}
		if item.OwnerUserID != account {
			return nil, 0, ErrAssignmentForbidden
		}
		out = append(out, item)
	}
	return out, total, nil
}
