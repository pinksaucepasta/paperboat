package machineservices

import (
	"context"
	"strconv"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
)

// ResolveBrowserTx consumes the same SQL authority as native grants and adds the
// existing browser team generation fence. It does not probe or activate a port.
func ResolveBrowserTx(ctx context.Context, tx *db.Tx, actor, machine string, port int) (Authority, teams.Decision, error) {
	rows, e := tx.Query(ctx, `SELECT source_kind,source_id,source_generation,coalesce(team_id,'') FROM machine_service_authorities($1,$2,$3) ORDER BY CASE WHEN team_id IS NULL THEN 0 ELSE 1 END,CASE WHEN source_kind='automatic' THEN 1 ELSE 0 END,source_kind,source_id,team_id`, actor, machine, port)
	if e != nil {
		return Authority{}, teams.Decision{}, e
	}
	var selected Authority
	found := false
	ambiguous := false
	for rows.Next() {
		var a Authority
		if e = rows.Scan(&a.SourceKind, &a.SourceID, &a.Generation, &a.TeamID); e != nil {
			rows.Close()
			return Authority{}, teams.Decision{}, e
		}
		if !found {
			selected = a
			found = true
		}
		if selected.TeamID != "" && selected.TeamID != a.TeamID {
			ambiguous = true
		}
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return Authority{}, teams.Decision{}, e
	}
	if !found || ambiguous {
		return Authority{}, teams.Decision{}, ErrDenied
	}
	if selected.TeamID == "" {
		return selected, teams.Decision{}, nil
	}
	if e = teams.Lock(ctx, tx); e != nil {
		return Authority{}, teams.Decision{}, e
	}
	kind, id, permission := selected.SourceKind, selected.SourceID, "use"
	if kind == "automatic" {
		kind, id, permission = "machine", machine, "private_access"
	}
	decision, e := teams.AuthorizeTx(ctx, tx, actor, selected.TeamID, kind, id, permission)
	return selected, decision, e
}
func (a Authority) Fingerprint() string {
	return a.SourceKind + ":" + a.SourceID + ":" + strconv.FormatInt(a.Generation, 10)
}
