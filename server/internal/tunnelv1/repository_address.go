package tunnelv1

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
)

// AddressCheck is CheckAddress's answer: Host is the label under the tunnel's base domain, Current that the tunnel
// already has it, Free that no resource other than the tunnel holds it and no active operator block names it.
type AddressCheck struct {
	Host    string
	Current bool
	Free    bool
}

// CheckAddress reads the stored (unmasked) endpoint. A tunnel's hostname is reserved in managed_hostnames for its
// whole life (and after deletion), so this covers tunnels, previews and machine hostnames alike.
func (r *SQLRepository) CheckAddress(ctx context.Context, accountID, tunnelID, label string) (AddressCheck, error) {
	var check AddressCheck
	err := r.db.Pool().QueryRow(ctx, `SELECT h.host, h.host=substring(t.stable_endpoint from 9),
 NOT EXISTS(SELECT 1 FROM paperboat.managed_hostnames WHERE hostname=h.host AND NOT (resource_kind='tunnel' AND resource_id=t.id))
 AND NOT EXISTS(SELECT 1 FROM paperboat.publication_blocks WHERE hostname=h.host AND blocked)
FROM paperboat.tunnels t, LATERAL (SELECT $3::text||substring(t.stable_endpoint from position('.' in t.stable_endpoint)) AS host) h
WHERE t.id=$2 AND t.account_id=$1`, accountID, tunnelID, label).Scan(&check.Host, &check.Current, &check.Free)
	return check, translate(err)
}

// moveAddressTx points the tunnel (already updated to its new generation) at newHost in the transaction that
// changes it: the hostname reservation, the stable endpoint, and the managed routes' match hostname with a new route
// generation (so the engine mirror, the edges and usage accounting see a new revision). The old name is released at
// once, like every reference product (zrok, headscale, frp); managed_hostnames' primary key makes the claim atomic
// across all accounts. An operator-blocked address cannot move, or a rename would shed the block.
func moveAddressTx(ctx context.Context, tx *db.Tx, tunnel dbsqlc.Tunnel, newHost, actorID string, now time.Time) (dbsqlc.Tunnel, error) {
	oldHost := strings.TrimPrefix(tunnel.StableEndpoint, "https://")
	var blocked, newBlocked bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.publication_blocks WHERE hostname=$1 AND blocked),
 EXISTS(SELECT 1 FROM paperboat.publication_blocks WHERE hostname=$2 AND blocked)`, oldHost, newHost).Scan(&blocked, &newBlocked); err != nil {
		return tunnel, translate(err)
	}
	if blocked {
		return tunnel, ErrAddressBlocked
	}
	if newBlocked {
		return tunnel, ErrSubdomainTaken
	}
	moved, err := tx.Exec(ctx, `UPDATE paperboat.managed_hostnames SET hostname=$2 WHERE hostname=$1 AND resource_kind='tunnel' AND resource_id=$3`, oldHost, newHost, tunnel.ID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return tunnel, ErrSubdomainTaken
		}
		return tunnel, translate(err)
	}
	if moved.RowsAffected() != 1 {
		return tunnel, ErrConflict
	}
	if _, err := tx.Exec(ctx, `UPDATE paperboat.tunnels SET stable_endpoint='https://'||$2 WHERE id=$1`, tunnel.ID, newHost); err != nil {
		if errors.Is(translate(err), ErrEndpointConflict) {
			return tunnel, ErrSubdomainTaken
		}
		return tunnel, translate(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE paperboat.tunnel_routes SET match_hostname=$3, generation=generation+1, updated_at=$4, updated_by_actor_id=$5
WHERE tunnel_id=$1 AND match_hostname=$2 AND deleted_at IS NULL`, tunnel.ID, oldHost, newHost, now, actorID); err != nil {
		return tunnel, translate(err)
	}
	tunnel.StableEndpoint = "https://" + newHost
	return tunnel, nil
}
