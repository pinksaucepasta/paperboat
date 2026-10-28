package usermachines

import (
	"context"
	"errors"

	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

// MachineSummary counts the machines of the selected workspace the account may see, as the machine list
// authorizes them. Removed machines are in neither count.
type MachineSummary struct {
	Total      int            `json:"total"`
	ByState    map[string]int `json:"by_state"`
	ByPlatform map[string]int `json:"by_platform"`
}

// Summary is one aggregate over the machine list's own authorization (inventoryFilter): the dashboard counts the
// whole workspace instead of the first page.
func (s *Service) Summary(ctx context.Context, account string) (MachineSummary, error) {
	out := MachineSummary{
		ByState:    map[string]int{"online": 0, "offline": 0, "pending": 0},
		ByPlatform: map[string]int{"linux": 0, "macos": 0, "windows": 0},
	}
	if scope, ok := workspace.FromContext(ctx); ok {
		if err := workspace.Validate(ctx, s.db, account, scope); err != nil {
			return MachineSummary{}, err
		}
	}
	with, predicate, args := inventoryFilter(ctx, account, InventoryOptions{})
	rows, err := s.db.SQL().QueryContext(ctx, with+`SELECT
 m.state,
 CASE WHEN m.platform='darwin' THEN 'macos' ELSE m.platform END,count(*)`+predicate+` GROUP BY 1,2`, args...)
	if err != nil {
		return MachineSummary{}, err
	}
	for rows.Next() {
		var state, platform string
		var n int
		if err = rows.Scan(&state, &platform, &n); err != nil {
			return MachineSummary{}, errors.Join(err, rows.Close())
		}
		out.ByState[state] += n
		out.Total += n
		out.ByPlatform[platform] += n
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return MachineSummary{}, err
	}
	return out, nil
}
