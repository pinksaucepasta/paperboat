package workspace

import (
	"context"

	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/teams"
)

type Item struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	Role string `json:"role"`
}

type Service struct{ db *db.DB }

func New(database *db.DB) *Service { return &Service{db: database} }

func (s *Service) Validate(ctx context.Context, account string, scope Scope) error {
	if s == nil || s.db == nil {
		return ErrForbidden
	}
	return Validate(ctx, s.db, account, scope)
}

func (s *Service) List(ctx context.Context, account string) ([]Item, error) {
	if s == nil || s.db == nil {
		return nil, ErrForbidden
	}
	out := []Item{{ID: "personal", Name: "Personal", Kind: "personal", Role: "owner"}}
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := teams.Lock(ctx, tx); err != nil {
			return err
		}
		var active bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1 AND status='active')`, account).Scan(&active); err != nil {
			return err
		}
		if !active {
			return ErrForbidden
		}
		rows, err := tx.Query(ctx, `SELECT t.team_id,coalesce(t.display_name,t.team_id),CASE WHEN t.owner_account=$1 THEN 'owner' ELSE m.role END
			FROM teams t JOIN team_members m USING(team_id)
			WHERE m.account_id=$1 AND m.active AND t.deleted_at IS NULL AND NOT t.suspended
			ORDER BY t.team_id LIMIT $2`, account, teams.MaximumMembers+1)
		if err != nil {
			return err
		}
		for rows.Next() {
			var teamID, name, role string
			if err := rows.Scan(&teamID, &name, &role); err != nil {
				rows.Close()
				return err
			}
			out = append(out, Item{ID: teamID, Name: name, Kind: "team", Role: role})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if len(out)-1 > teams.MaximumMembers {
			return teams.ErrLimit
		}
		return nil
	})
	return out, err
}
