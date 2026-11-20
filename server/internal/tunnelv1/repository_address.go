package tunnelv1

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	tunnelshared "github.com/pinksaucepasta/paperboat/shared/tunnelv1"
)

// AddressCheck is CheckAddress's answer: Host is the label under the tunnel's base domain, Current that the tunnel
// already has it, Free that no resource other than the tunnel holds it and no active operator block names it.
type AddressCheck struct {
	Host    string
	Current bool
	Free    bool
	// SelfHosted: the tunnel's workspace serves its tunnels on its own edges and domains, so it has no managed
	// address to change.
	SelfHosted bool
}

// CheckAddress reads the stored (unmasked) endpoint. A tunnel's hostname is reserved in managed_hostnames for its
// whole life (and after deletion), so this covers tunnels, previews and machine hostnames alike; a name another
// tunnel is moving to or just left (tunnel_address_holds) is not free either.
func (r *SQLRepository) CheckAddress(ctx context.Context, accountID, tunnelID, label string) (AddressCheck, error) {
	var check AddressCheck
	err := r.db.Pool().QueryRow(ctx, `SELECT h.host, h.host=substring(t.stable_endpoint from 9),
 NOT EXISTS(SELECT 1 FROM paperboat.managed_hostnames WHERE hostname=h.host AND NOT (resource_kind='tunnel' AND resource_id=t.id))
 AND NOT EXISTS(SELECT 1 FROM paperboat.publication_blocks WHERE hostname=h.host AND blocked)
 AND NOT EXISTS(SELECT 1 FROM paperboat.tunnel_address_holds WHERE hostname=h.host AND (kind='incoming' OR (until>now() AND tunnel_id<>t.id))),
 paperboat.workspace_edge_mode(t.account_id,paperboat.publication_workspace_team('tunnel',t.id))='self-hosted-only'
FROM paperboat.tunnels t, LATERAL (SELECT $3::text||substring(t.stable_endpoint from position('.' in t.stable_endpoint)) AS host) h
WHERE t.id=$2 AND t.account_id=$1`, accountID, tunnelID, label).Scan(&check.Host, &check.Current, &check.Free, &check.SelfHosted)
	return check, translate(err)
}

// AddressPolicy bounds an address change (configuration: tunnel.address_move_timeout, tunnel.address_cooldown).
type AddressPolicy struct {
	// MoveTimeout is how long the edges get to serve the new name next to the old one before the change is undone.
	MoveTimeout time.Duration
	// Cooldown is how long the old name stays reserved to the tunnel after it stops serving it.
	Cooldown time.Duration
}

func (p AddressPolicy) valid() bool { return p.MoveTimeout > 0 && p.Cooldown > 0 }

// startAddressMoveTx begins moving the tunnel (already updated to its new generation) to newHost, make before
// break. The new name is claimed in tunnel_address_holds (primary key: unique across accounts; managed_hostnames
// still names the old one) and, when edges serve the tunnel, its managed HTTP routes get a new revision: the edge
// snapshot lists the claimed name as a second name of each route (ListTunnelEdgeRouteAssignmentsForNodeV1), so
// the old and the new address serve together. SettleAddressMoves finishes the move once every live edge serves
// both. With nothing serving there is nothing to keep up, and the move finishes here. An operator-blocked address
// cannot move, or a rename would shed the block.
func startAddressMoveTx(ctx context.Context, tx *db.Tx, tunnel dbsqlc.Tunnel, newHost, actorID, operationID string, policy AddressPolicy, now time.Time) (dbsqlc.Tunnel, bool, error) {
	if !policy.valid() {
		return tunnel, false, fmt.Errorf("%w: the address policy is not configured", ErrInvalidInput)
	}
	// The same lock every tunnel route write takes (lock_shared_443_names_v1): the claim below and a concurrent
	// move that finishes onto the same name see each other.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(1885686843)`); err != nil {
		return tunnel, false, translate(err)
	}
	oldHost := strings.TrimPrefix(tunnel.StableEndpoint, "https://")
	var blocked, newBlocked, pending, inManagedNames bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.publication_blocks WHERE hostname=$1 AND blocked),
 EXISTS(SELECT 1 FROM paperboat.publication_blocks WHERE hostname=$2 AND blocked),
 EXISTS(SELECT 1 FROM paperboat.tunnel_address_holds WHERE tunnel_id=$3 AND kind='incoming'),
 EXISTS(SELECT 1 FROM paperboat.managed_hostnames WHERE hostname=$2)`, oldHost, newHost, tunnel.ID).Scan(&blocked, &newBlocked, &pending, &inManagedNames); err != nil {
		return tunnel, false, translate(err)
	}
	switch {
	case blocked:
		return tunnel, false, ErrAddressBlocked
	case pending:
		return tunnel, false, ErrAddressChangePending
	case newBlocked || inManagedNames:
		return tunnel, false, ErrSubdomainTaken
	}
	// A hold of another tunnel blocks the claim: a move in progress to the name is never taken over, not even one past
	// its deadline (SettleAddressMoves undoes it and fails its operation first, so no operation can report a change
	// that did not happen), and a cooldown only once it has ended. The tunnel's own holds (it may take its old name
	// back) are replaced.
	claimed, err := tx.Exec(ctx, `INSERT INTO paperboat.tunnel_address_holds(hostname,tunnel_id,operation_id,kind,until) VALUES($1,$2,$5,'incoming',$3)
ON CONFLICT(hostname) DO UPDATE SET tunnel_id=EXCLUDED.tunnel_id,operation_id=EXCLUDED.operation_id,kind='incoming',until=EXCLUDED.until,created_at=$4
WHERE paperboat.tunnel_address_holds.tunnel_id=EXCLUDED.tunnel_id
 OR (paperboat.tunnel_address_holds.kind='retired' AND paperboat.tunnel_address_holds.until<=$4)`, newHost, tunnel.ID, now.Add(policy.MoveTimeout), now, operationID)
	if err != nil {
		return tunnel, false, translate(err)
	}
	if claimed.RowsAffected() != 1 {
		return tunnel, false, ErrSubdomainTaken
	}
	var serving bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.tunnel_routes tr JOIN paperboat.endpoint_routes er ON er.tunnel_route_id=tr.id
 JOIN paperboat.endpoint_edge_assignments x ON x.route_id=er.id AND x.desired='active'
 WHERE tr.tunnel_id=$1 AND tr.protocol='http' AND tr.match_type='managed' AND tr.match_hostname=$2 AND tr.desired_state='active' AND tr.deleted_at IS NULL)`,
		tunnel.ID, oldHost).Scan(&serving); err != nil {
		return tunnel, false, translate(err)
	}
	if !serving {
		tunnel, err = finishAddressMoveTx(ctx, tx, tunnel, newHost, actorID, policy, now)
		return tunnel, false, err
	}
	if err := touchAddressRoutesTx(ctx, tx, tunnel.ID, oldHost, actorID, now); err != nil {
		return tunnel, false, err
	}
	return tunnel, true, nil
}

// touchAddressRoutesTx gives the tunnel's managed HTTP routes at host a new revision, so every edge fetches and
// confirms a snapshot that has (or no longer has) the move's second name.
func touchAddressRoutesTx(ctx context.Context, tx *db.Tx, tunnelID, host, actorID string, now time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE paperboat.tunnel_routes SET generation=generation+1, updated_at=$3, updated_by_actor_id=$4
WHERE tunnel_id=$1 AND protocol='http' AND match_type='managed' AND match_hostname=$2 AND desired_state='active' AND deleted_at IS NULL`, tunnelID, host, now, actorID)
	return translate(err)
}

// finishAddressMoveTx points the tunnel (already at its new generation) at newHost: the hostname reservation, the
// stable endpoint, and the routes' match hostname with a new route generation (so the engine mirror, the edges and
// usage accounting see a new revision). managed_hostnames' primary key makes the claim atomic across all accounts.
// The old name stays reserved to the tunnel for the cooldown (zrok, headscale and frp release at once; a visitor
// with the old link must not land on another account's tunnel meanwhile).
func finishAddressMoveTx(ctx context.Context, tx *db.Tx, tunnel dbsqlc.Tunnel, newHost, actorID string, policy AddressPolicy, now time.Time) (dbsqlc.Tunnel, error) {
	oldHost := strings.TrimPrefix(tunnel.StableEndpoint, "https://")
	var newBlocked bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.publication_blocks WHERE hostname=$1 AND blocked)`, newHost).Scan(&newBlocked); err != nil {
		return tunnel, translate(err)
	}
	if newBlocked {
		return tunnel, ErrAddressBlocked // an operator blocked the name while the move was in progress
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
	if _, err := tx.Exec(ctx, `DELETE FROM paperboat.tunnel_address_holds WHERE hostname=$1 AND tunnel_id=$2 AND kind='incoming'`, newHost, tunnel.ID); err != nil {
		return tunnel, translate(err)
	}
	retired, err := tx.Exec(ctx, `INSERT INTO paperboat.tunnel_address_holds(hostname,tunnel_id,kind,until) VALUES($1,$2,'retired',$3)
ON CONFLICT(hostname) DO UPDATE SET tunnel_id=EXCLUDED.tunnel_id,operation_id=NULL,kind='retired',until=EXCLUDED.until,created_at=$4
WHERE paperboat.tunnel_address_holds.tunnel_id=EXCLUDED.tunnel_id OR paperboat.tunnel_address_holds.until<=$4`, oldHost, tunnel.ID, now.Add(policy.Cooldown), now)
	if err != nil {
		return tunnel, translate(err)
	}
	if retired.RowsAffected() != 1 {
		return tunnel, ErrConflict // another tunnel holds the old name: moving off it without its cooldown would hand it over
	}
	tunnel.StableEndpoint = "https://" + newHost
	return tunnel, nil
}

// AddressSettleRecord is one pass of SettleAddressMoves.
type AddressSettleRecord struct {
	Now           time.Time
	Limit         int
	Policy        AddressPolicy
	ActorID       string
	ActorType     string
	RequestID     string
	CorrelationID string
	NewID         func(prefix string) (string, error)
}

// SettleAddressMoves finishes the address moves whose new name every live edge now serves next to the old one, and
// undoes the ones that did not get there in time (or whose tunnel is gone): the old name stops serving and is
// held for the cooldown, or the new name is dropped and the tunnel's operation fails. It also forgets expired
// cooldowns. One move failing does not hold back the others.
func (r *SQLRepository) SettleAddressMoves(ctx context.Context, input AddressSettleRecord) error {
	if input.Now.IsZero() {
		input.Now = r.now().UTC()
	}
	if input.Limit < 1 || input.Limit > 200 || !input.Policy.valid() || strings.TrimSpace(input.ActorID) == "" || !validActorType(input.ActorType) {
		return ErrInvalidInput
	}
	if input.NewID == nil {
		input.NewID = randomID
	}
	rows, err := r.db.Pool().Query(ctx, `SELECT hostname FROM paperboat.tunnel_address_holds WHERE kind='incoming' ORDER BY until LIMIT $1`, input.Limit)
	if err != nil {
		return translate(err)
	}
	var hosts []string
	for rows.Next() {
		var host string
		if err := rows.Scan(&host); err != nil {
			rows.Close()
			return err
		}
		hosts = append(hosts, host)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	var failures error
	for _, host := range hosts {
		failures = errors.Join(failures, r.settleAddressMove(ctx, host, input))
	}
	if _, err := r.db.Pool().Exec(ctx, `DELETE FROM paperboat.tunnel_address_holds WHERE hostname IN
(SELECT h.hostname FROM paperboat.tunnel_address_holds h WHERE (h.kind='retired' AND h.until<$1)
 OR NOT EXISTS(SELECT 1 FROM paperboat.tunnels t WHERE t.id=h.tunnel_id) LIMIT 100)`, input.Now); err != nil {
		failures = errors.Join(failures, translate(err))
	}
	return failures
}

func (r *SQLRepository) settleAddressMove(ctx context.Context, newHost string, input AddressSettleRecord) error {
	machine, err := r.addressMachine(ctx, newHost)
	if err != nil {
		return err
	}
	err = r.inMachineTx(ctx, machine, func(ctx context.Context, tx *db.Tx) error {
		return settleAddressMoveTx(ctx, tx, newHost, input, "")
	})
	if reason := undoReason(err); reason != "" {
		// The new name cannot be taken any more (blocked, or claimed by another resource meanwhile): undo the move.
		err = r.inMachineTx(ctx, machine, func(ctx context.Context, tx *db.Tx) error {
			return settleAddressMoveTx(ctx, tx, newHost, input, reason)
		})
	}
	return translate(err)
}

// The error codes of an undone move's operation (the console shows them next to the address).
const (
	moveNotReady = "tunnel_address_not_ready" // the edges did not serve the new name before the deadline
	moveTaken    = "subdomain_taken"
	moveBlocked  = "address_blocked"
	movePaused   = "tunnel_paused"
	moveDeleted  = "tunnel_deleted"
)

func undoReason(err error) string {
	switch {
	case errors.Is(err, ErrAddressBlocked):
		return moveBlocked
	case errors.Is(err, ErrSubdomainTaken), errors.Is(err, ErrConflict):
		return moveTaken
	}
	return ""
}

func settleAddressMoveTx(ctx context.Context, tx *db.Tx, newHost string, input AddressSettleRecord, undoCode string) error {
	if err := authorizeTunnelManagementTx(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(1885686843)`); err != nil {
		return translate(err)
	}
	var tunnelID, accountID, operationID string
	var until time.Time
	err := tx.QueryRow(ctx, `SELECT h.tunnel_id,t.account_id,coalesce(h.operation_id,''),h.until FROM paperboat.tunnel_address_holds h JOIN paperboat.tunnels t ON t.id=h.tunnel_id
WHERE h.hostname=$1 AND h.kind='incoming' FOR UPDATE OF h SKIP LOCKED`, newHost).Scan(&tunnelID, &accountID, &operationID, &until)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // settled by another pass, or the tunnel row is gone
	}
	if err != nil {
		return translate(err)
	}
	queries := tx.Queries()
	tunnel, err := queries.GetPreviewTunnel(ctx, dbsqlc.GetPreviewTunnelParams{ID: tunnelID, AccountID: accountID})
	if err != nil {
		return translate(err)
	}
	oldHost := strings.TrimPrefix(tunnel.StableEndpoint, "https://")
	failOperation := func(code string) error {
		_, err := tx.Exec(ctx, `UPDATE paperboat.operations SET state='failed',phase='failed',progress=100,retrying=false,next_retry_at=NULL,error_code=$3,result_reference=$4,completed_at=$2,updated_at=$2
WHERE id=$1 AND state IN ('pending','running')`, operationID, input.Now, code, newHost)
		return translate(err)
	}
	if tunnel.DesiredState == DesiredDeleted {
		// Nothing serves the tunnel any more; the move is moot.
		if _, err := tx.Exec(ctx, `DELETE FROM paperboat.tunnel_address_holds WHERE hostname=$1 AND tunnel_id=$2 AND kind='incoming'`, newHost, tunnelID); err != nil {
			return translate(err)
		}
		return failOperation(moveDeleted)
	}
	if tunnel.DesiredState != DesiredActive && undoCode == "" {
		undoCode = movePaused // a paused tunnel has no edge to confirm the new name: undo now instead of waiting out the timeout
	}
	ready := false
	if undoCode == "" {
		if err := tx.QueryRow(ctx, `SELECT coalesce(bool_and(paperboat.endpoint_route_ready(er.id,$3,true)),false)
FROM paperboat.tunnel_routes tr JOIN paperboat.endpoint_routes er ON er.tunnel_route_id=tr.id
WHERE tr.tunnel_id=$1 AND tr.protocol='http' AND tr.match_type='managed' AND tr.match_hostname=$2 AND tr.desired_state='active' AND tr.deleted_at IS NULL`,
			tunnelID, oldHost, input.Now).Scan(&ready); err != nil {
			return translate(err)
		}
	}
	if !ready && undoCode == "" && input.Now.Before(until) {
		return nil
	}
	tunnel, err = queries.BumpTunnelGenerationForResourceV1(ctx, dbsqlc.BumpTunnelGenerationForResourceV1Params{TunnelID: tunnelID, AccountID: accountID, Now: input.Now})
	if err != nil {
		return translate(err)
	}
	eventType, outcome, metadata := "tunnel.address_changed", "changed", map[string]any{"previous_hostname": oldHost, "hostname": newHost}
	if ready {
		if tunnel, err = finishAddressMoveTx(ctx, tx, tunnel, newHost, input.ActorID, input.Policy, input.Now); err != nil {
			return err
		}
	} else {
		eventType, outcome = "tunnel.address_change_failed", "unchanged"
		if _, err := tx.Exec(ctx, `DELETE FROM paperboat.tunnel_address_holds WHERE hostname=$1 AND tunnel_id=$2 AND kind='incoming'`, newHost, tunnelID); err != nil {
			return translate(err)
		}
		if err := touchAddressRoutesTx(ctx, tx, tunnelID, oldHost, input.ActorID, input.Now); err != nil {
			return err
		}
		if undoCode == "" {
			undoCode = moveNotReady
		}
		if err := failOperation(undoCode); err != nil {
			return err
		}
	}
	if err := createResourceConfigGeneration(ctx, queries, tunnel, input.ActorID, input.Now); err != nil {
		return err
	}
	if err := syncEngine(ctx, tx, tunnel); err != nil {
		return err
	}
	auditID, err := input.NewID("event")
	if err != nil {
		return err
	}
	metadata["generation"] = tunnel.Generation
	return insertAudit(ctx, queries, auditID, accountID, input.ActorID, input.ActorType, eventType, "update", outcome, tunnelID, "", input.RequestID, input.CorrelationID, "", input.Now, metadata)
}

// AddressChanges returns the address change in progress, or the one that failed last, of each tunnel of the account
// that has one (keyed by tunnel id). The target of a failed move is the operation's result reference. A failure counts until the tunnel's next patch operation: any later change
// to the tunnel (or a new address change) replaces it.
func (r *SQLRepository) AddressChanges(ctx context.Context, accountID string, tunnelIDs []string) (map[string]tunnelshared.AddressChange, error) {
	if len(tunnelIDs) == 0 {
		return nil, nil
	}
	rows, err := r.db.Pool().Query(ctx, `SELECT h.tunnel_id,'moving',h.hostname,'',h.created_at
FROM paperboat.tunnel_address_holds h JOIN paperboat.tunnels t ON t.id=h.tunnel_id
WHERE t.account_id=$1 AND h.tunnel_id=ANY($2::text[]) AND h.kind='incoming'
UNION ALL
SELECT o.resource_id,'failed',coalesce(o.result_reference,''),o.error_code,o.completed_at
FROM (SELECT DISTINCT ON (resource_id) resource_id,state,error_code,result_reference,completed_at FROM paperboat.operations
 WHERE account_id=$1 AND resource_kind='tunnel' AND resource_id=ANY($2::text[]) AND operation_type='tunnel.patch'
 ORDER BY resource_id,created_at DESC,id DESC) o
WHERE o.state='failed' AND o.error_code IN ('tunnel_address_not_ready','subdomain_taken','address_blocked','tunnel_paused','tunnel_deleted')`, accountID, tunnelIDs)
	if err != nil {
		return nil, translate(err)
	}
	defer rows.Close()
	changes := map[string]tunnelshared.AddressChange{}
	for rows.Next() {
		var id string
		var change tunnelshared.AddressChange
		var at *time.Time
		if err := rows.Scan(&id, &change.State, &change.Hostname, &change.ReasonCode, &at); err != nil {
			return nil, translate(err)
		}
		if at != nil {
			change.UpdatedAt = at.UTC()
		}
		changes[id] = change
	}
	return changes, translate(rows.Err())
}
