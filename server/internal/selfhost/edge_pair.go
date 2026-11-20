package selfhost

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/edgeset"
	"github.com/pinksaucepasta/paperboat/server/internal/machineevents"
	"github.com/pinksaucepasta/paperboat/server/internal/machinelatency"
)

// InvalidPairError is ErrInvalid with the sentence a user is shown (regions are named, never nodes).
type InvalidPairError struct{ Message string }

func (e *InvalidPairError) Error() string { return ErrInvalid.Error() + ": " + e.Message }
func (e *InvalidPairError) Unwrap() error { return ErrInvalid }

// ErrEdgePairUnassigned means no edge can serve the workspace yet, so it has no pair to show.
var ErrEdgePairUnassigned = errors.New("the workspace has no edge pair yet")

// PairRegion is one region of a workspace's pair. Users see regions only: which machines of a region serve them is
// the server's business (prior art: tailscale tailcfg.DERPRegion, a region is a set of nodes the client does not
// choose between). Name is Paperboat's display name for a managed region and the region id for the owner's own.
type PairRegion struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	Status        string     `json:"status"` // ready | draining | unavailable, folded over the region's nodes
	BestRTTMillis *int       `json:"best_rtt_ms,omitempty"`
	MeasuredAt    *time.Time `json:"measured_at,omitempty"`
}

// EdgePair is the primary and backup region of a workspace. Fallback is absent while only one region can serve it.
type EdgePair struct {
	Kind     string      `json:"kind"` // managed | self_hosted
	Primary  PairRegion  `json:"primary"`
	Fallback *PairRegion `json:"fallback,omitempty"`
}

// pairScope is the workspace and the account that stands for it when asking which edges it may use.
func (s *Service) pairScope(ctx context.Context, actor string, scope Scope, write bool) (edgeset.Scope, error) {
	scope, err := s.authorizeScope(ctx, actor, scope, write, false)
	if err != nil {
		return edgeset.Scope{}, err
	}
	if scope.Kind != "account" && scope.Kind != "team" {
		return edgeset.Scope{}, ErrInvalid
	}
	out := edgeset.Scope{Kind: scope.Kind, ID: scope.ID, Account: actor}
	if scope.Kind == "team" {
		if err = s.database.SQL().QueryRowContext(ctx, `SELECT owner_account FROM paperboat.teams WHERE team_id=$1`, scope.ID).Scan(&out.Account); err != nil {
			return edgeset.Scope{}, err
		}
	}
	return out, nil
}

// EdgePair reads the workspace's stored pair of regions (assigning one first when a machine has not yet).
func (s *Service) EdgePair(ctx context.Context, actor string, scope Scope) (EdgePair, error) {
	ws, err := s.pairScope(ctx, actor, scope, false)
	if err != nil {
		return EdgePair{}, err
	}
	var mode string
	var regions []string
	err = s.database.InReadCommittedTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := edgeset.EnsureTx(ctx, tx, ws, s.now().UTC()); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT mode,regions FROM workspace_edge_sets WHERE scope_kind=$1 AND scope_id=$2`, ws.Kind, ws.ID).Scan(&mode, &regions)
	})
	if errors.Is(err, pgx.ErrNoRows) || err == nil && len(regions) == 0 {
		return EdgePair{}, ErrEdgePairUnassigned
	}
	if err != nil {
		return EdgePair{}, err
	}
	return s.readPair(ctx, actor, ws, mode, regions)
}

func workspaceTeam(ws edgeset.Scope) any {
	if ws.Kind == "team" {
		return ws.ID
	}
	return nil
}

// readPair describes the first two regions of a stored pair, with the best RTT any of the account's machines
// measured to a node of each.
func (s *Service) readPair(ctx context.Context, actor string, ws edgeset.Scope, mode string, regions []string) (EdgePair, error) {
	if len(regions) > edgeset.Size {
		regions = regions[:edgeset.Size]
	}
	pair := EdgePair{Kind: "managed"}
	if mode == "self-hosted-only" {
		pair.Kind = "self_hosted"
	}
	read := make([]PairRegion, 0, len(regions))
	for _, id := range regions {
		r, err := s.readRegion(ctx, actor, ws, pair.Kind, id)
		if err != nil {
			return EdgePair{}, err
		}
		read = append(read, r)
	}
	if len(read) == 0 {
		return EdgePair{}, ErrEdgePairUnassigned
	}
	pair.Primary = read[0]
	if len(read) > 1 {
		pair.Fallback = &read[1]
	}
	return pair, nil
}

// readRegion folds the status of the workspace's nodes in one region (the nodes the workspace may use under its
// current mode: ready if any is, else draining if any is, else unavailable) and takes the best RTT measured to any
// of them. Only the region id and the display name leave this function: never a node id, host or heartbeat.
func (s *Service) readRegion(ctx context.Context, actor string, ws edgeset.Scope, kind, id string) (PairRegion, error) {
	now := s.now().UTC()
	out := PairRegion{ID: id, Name: id, Status: "unavailable"}
	if kind == "managed" {
		if err := s.database.SQL().QueryRowContext(ctx, `SELECT coalesce((SELECT display_name FROM paperboat.operator_regions WHERE id=$1),$1)`, id).Scan(&out.Name); err != nil {
			return PairRegion{}, err
		}
	}
	rows, err := s.database.SQL().QueryContext(ctx, `SELECT n.id,`+edgeset.NodeStatusSQL("$2")+` FROM paperboat.control_tunnel_nodes n
WHERE n.region=$1 AND n.roles @> ARRAY['edge']::text[] AND n.state<>'retired' AND paperboat.node_workspace_edge_candidate(n.id,$3,$4)
ORDER BY n.id LIMIT 100`, id, now, ws.Account, workspaceTeam(ws))
	if err != nil {
		return PairRegion{}, err
	}
	defer rows.Close()
	var nodeIDs []string
	rank := map[string]int{"ready": 0, "draining": 1, "unavailable": 2}
	for rows.Next() {
		var node, status string
		if err = rows.Scan(&node, &status); err != nil {
			return PairRegion{}, err
		}
		nodeIDs = append(nodeIDs, node)
		if rank[status] < rank[out.Status] {
			out.Status = status
		}
	}
	if err = rows.Err(); err != nil {
		return PairRegion{}, err
	}
	best, err := machinelatency.BestForAccount(ctx, s.database.SQL(), actor, nodeIDs, now)
	if err != nil {
		return PairRegion{}, err
	}
	for _, b := range best {
		if out.BestRTTMillis == nil || b.RTTMillis < *out.BestRTTMillis {
			rtt, at := b.RTTMillis, b.MeasuredAt
			out.BestRTTMillis, out.MeasuredAt = &rtt, &at
		}
	}
	return out, nil
}

// SetEdgePair makes primary and fallback (region ids) the workspace's pair through the machinery that already
// assigns edges: the tunnel pool policy (both Paperboat's regions or both the workspace's own), the stored pair
// that placement consults, and the tunnels/previews topics that make its machines look again. Switching between
// Paperboat's regions and its own servers is refused while previews or tunnels run (LivePublicationsError), as the
// pool is. Errors name regions, never nodes.
func (s *Service) SetEdgePair(ctx context.Context, actor string, scope Scope, kind, primary, fallback string) (EdgePair, error) {
	if kind != "managed" && kind != "self_hosted" {
		return EdgePair{}, &InvalidPairError{"kind must be managed or self_hosted."}
	}
	for _, region := range []string{primary, fallback} {
		if !validRegion(region) {
			return EdgePair{}, &InvalidPairError{"primary_region and fallback_region must be region names."}
		}
	}
	if primary == fallback {
		return EdgePair{}, &InvalidPairError{"Choose two different regions."}
	}
	ws, err := s.pairScope(ctx, actor, scope, true)
	if err != nil {
		return EdgePair{}, err
	}
	var mode string
	err = s.database.InTx(ctx, func(ctx context.Context, tx *db.Tx) (storeErr error) {
		mode, storeErr = s.storePair(ctx, tx, actor, ws, kind, []string{primary, fallback})
		return storeErr
	})
	if err != nil {
		return EdgePair{}, err
	}
	// The pair is stored; a failed wake-up only leaves machines to see it when their attachment next refreshes.
	if err = s.publishPairChange(ctx, ws); err != nil {
		slog.ErrorContext(ctx, "edge_pair_publish_failed", "scope_kind", ws.Kind, "scope_id", ws.ID, "error", err)
	}
	return s.readPair(ctx, actor, ws, mode, []string{primary, fallback})
}

// storePair writes the policy, the pair and (for the workspace's own servers) the pool, in this order: the pair
// is stored before anything resolves edges (repointing the domains, judging the regions), because those go
// through node_workspace_edge_eligible, which follows the stored pair.
func (s *Service) storePair(ctx context.Context, tx *db.Tx, actor string, ws edgeset.Scope, kind string, regions []string) (string, error) {
	now := s.now().UTC()
	// The same lock the pool policy takes, so a pool change and a pair change never interleave.
	if err := edgeset.LockWorkspace(ctx, tx, ws); err != nil {
		return "", err
	}
	team := workspaceTeam(ws)
	if err := requireTeamManager(ctx, tx, actor, Scope{Kind: ws.Kind, ID: ws.ID}); err != nil {
		return "", err
	}
	mode := "managed-only"
	if kind == "self_hosted" {
		mode = "self-hosted-only"
	}
	var current string
	if err := tx.QueryRow(ctx, `SELECT paperboat.workspace_edge_mode($1,$2)`, ws.Account, team).Scan(&current); err != nil {
		return "", err
	}
	if current != mode {
		if err := refuseLiveEdgeChange(ctx, tx, ws.Account, team); err != nil {
			return "", err
		}
	}
	// The pool is the pair: the policy decides which servers may serve the workspace (every own server of the pair's
	// regions, filled in below), the stored pair which regions do.
	if _, err := tx.Exec(ctx, `INSERT INTO paperboat.selfhost_pool_policies(scope_kind,scope_id,capability,mode,installation_ids) VALUES($1,$2,'tunnel',$3,'{}')
ON CONFLICT(scope_kind,scope_id,capability) DO UPDATE SET mode=excluded.mode,installation_ids=excluded.installation_ids`, ws.Kind, ws.ID, mode); err != nil {
		return "", err
	}
	if err := edgeset.Store(ctx, tx, ws, mode, regions, now); err != nil {
		return "", err
	}
	if err := syncTunnelPool(ctx, tx, ws); err != nil {
		return "", err
	}
	if current != mode {
		if err := repointWorkspaceDomains(ctx, tx, ws.Account, team, mode); err != nil {
			return "", err
		}
	}
	// Eligibility is judged under the new mode, and a region nobody has heard from lately would be replaced by the
	// next reconcile, silently undoing the choice.
	for _, region := range regions {
		if err := s.requireRegionReady(ctx, tx, ws, kind, region, now); err != nil {
			return "", err
		}
	}
	return mode, nil
}

// requireRegionReady refuses a region the workspace cannot be served from right now: for Paperboat's it must be
// an enabled operator region, for both it needs a node the workspace may use (node_workspace_edge_candidate: the
// pair being decided cannot be a condition of itself) with a heartbeat inside edgeset.ReplaceAfter.
func (s *Service) requireRegionReady(ctx context.Context, tx *db.Tx, ws edgeset.Scope, kind, region string, now time.Time) error {
	var name string
	var ok bool
	if err := tx.QueryRow(ctx, `SELECT coalesce((SELECT display_name FROM paperboat.operator_regions WHERE id=$1),$1),
 ($6='self_hosted' OR EXISTS(SELECT 1 FROM paperboat.operator_regions WHERE id=$1 AND enabled))
 AND EXISTS(SELECT 1 FROM paperboat.control_tunnel_nodes n WHERE n.region=$1 AND n.roles @> ARRAY['edge']::text[]
  AND paperboat.node_workspace_edge_candidate(n.id,$2,$3) AND n.last_heartbeat_at>$4::timestamptz-make_interval(secs=>$5::float8))`,
		region, ws.Account, workspaceTeam(ws), now, edgeset.ReplaceAfter.Seconds(), kind).Scan(&name, &ok); err != nil {
		return err
	}
	if kind == "self_hosted" {
		name = region
	}
	if !ok {
		return &InvalidPairError{name + " is not available to this workspace right now; choose a region that is ready."}
	}
	return nil
}

// pairPublishBatch bounds how many machines one wake-up read and one topic write cover.
var pairPublishBatch = 1000

// publishPairChange wakes the workspace's machines on the topics their runtime watches for edge changes, so each
// looks at its attachment again instead of waiting for it to expire. It pages through the machines by id, after the
// pair is committed, so a workspace with many machines never holds the write transaction open.
func (s *Service) publishPairChange(ctx context.Context, ws edgeset.Scope) error {
	pool := s.database.Pool()
	after := ""
	for {
		rows, err := pool.Query(ctx, `SELECT id FROM paperboat.user_machines WHERE deleted_at IS NULL AND revoked_at IS NULL AND id>$3
 AND CASE WHEN $1='team' THEN owner_team_id=$2 ELSE user_id=$2 AND owner_team_id IS NULL END ORDER BY id LIMIT $4`, ws.Kind, ws.ID, after, pairPublishBatch)
		if err != nil {
			return fmt.Errorf("list machines to wake: %w", err)
		}
		machines := make([]string, 0, pairPublishBatch)
		for rows.Next() {
			var id string
			if err = rows.Scan(&id); err != nil {
				break
			}
			machines = append(machines, id)
		}
		rows.Close()
		if err == nil {
			err = rows.Err()
		}
		if err != nil {
			return fmt.Errorf("list machines to wake: %w", err)
		}
		for _, topic := range []string{machineevents.TopicTunnels, machineevents.TopicPreviews} {
			if err = machineevents.Publish(ctx, pool, machines, topic); err != nil {
				return fmt.Errorf("wake machines on %s: %w", topic, err)
			}
		}
		if len(machines) < pairPublishBatch {
			return nil
		}
		after = machines[len(machines)-1]
	}
}
