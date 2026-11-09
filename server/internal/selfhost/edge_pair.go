package selfhost

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/edgeset"
	"github.com/pinksaucepasta/paperboat/server/internal/machineevents"
	"github.com/pinksaucepasta/paperboat/server/internal/machinelatency"
)

// ErrEdgePairUnassigned means no edge can serve the workspace yet, so it has no pair to show.
var ErrEdgePairUnassigned = errors.New("the workspace has no edge pair yet")

// PairEdge is one edge of a workspace's pair. A self-hosted edge is named by its installation id (what
// PUT takes back), a Paperboat edge by its node id.
type PairEdge struct {
	ID            string     `json:"id"`
	Kind          string     `json:"kind"` // managed | self_hosted
	Region        string     `json:"region"`
	Site          string     `json:"site"`           // the operator-set site, else the region's display name; a self-hosted server shows its name
	Name          string     `json:"name,omitempty"` // a self-hosted server's name
	EndpointHost  string     `json:"endpoint_host,omitempty"`
	Status        string     `json:"status"`
	LastHeartbeat *time.Time `json:"last_heartbeat_at,omitempty"`
	BestRTTMillis *int       `json:"best_rtt_ms,omitempty"`
	MeasuredAt    *time.Time `json:"measured_at,omitempty"`
}

// EdgePair is the primary and fallback edge of a workspace. Fallback is absent while only one edge can serve it.
type EdgePair struct {
	Primary  PairEdge  `json:"primary"`
	Fallback *PairEdge `json:"fallback,omitempty"`
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

// EdgePair reads the workspace's stored edge set (assigning one first when a machine has not yet).
func (s *Service) EdgePair(ctx context.Context, actor string, scope Scope) (EdgePair, error) {
	ws, err := s.pairScope(ctx, actor, scope, false)
	if err != nil {
		return EdgePair{}, err
	}
	var ids []string
	err = s.database.InReadCommittedTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := edgeset.EnsureTx(ctx, tx, ws, s.now().UTC()); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT node_ids FROM workspace_edge_sets WHERE scope_kind=$1 AND scope_id=$2`, ws.Kind, ws.ID).Scan(&ids)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return EdgePair{}, ErrEdgePairUnassigned
	}
	if err != nil {
		return EdgePair{}, err
	}
	return s.readPair(ctx, actor, ws, ids)
}

// readPair describes the first two nodes of an edge set: the pair's edges, with the best RTT any of the account's
// machines measured to each.
func (s *Service) readPair(ctx context.Context, actor string, ws edgeset.Scope, ids []string) (EdgePair, error) {
	if len(ids) > 2 {
		ids = ids[:2]
	}
	now := s.now().UTC()
	edges, nodeIDs := make([]PairEdge, 0, 2), make([]string, 0, 2)
	for _, id := range ids {
		var e PairEdge
		var name, installation, site string
		var heartbeat sql.NullTime
		err := s.database.SQL().QueryRowContext(ctx, `SELECT n.id,coalesce(n.region,''),coalesce(n.endpoint_host,''),`+edgeset.NodeStatusSQL("$2")+`,n.last_heartbeat_at,
 coalesce(i.id,''),coalesce(i.name,''),coalesce(n.site,(SELECT r.display_name FROM paperboat.managed_node_authority m JOIN paperboat.operator_regions r ON r.id=m.region_id WHERE m.node_id=n.id AND m.retired_at IS NULL),'')
FROM paperboat.control_tunnel_nodes n
LEFT JOIN LATERAL (SELECT i.id,i.name FROM paperboat.selfhost_installations i WHERE i.node_id=n.id AND i.capability='tunnel' AND i.state='active' AND i.scope_kind<>'global'
 AND ((i.scope_kind=$3 AND i.scope_id=$4) OR ($3='account' AND paperboat.selfhost_scope_visible(i.scope_kind,i.scope_id,$5))) ORDER BY i.id LIMIT 1) i ON true
WHERE n.id=$1`, id, now, ws.Kind, ws.ID, ws.Account).Scan(&e.ID, &e.Region, &e.EndpointHost, &e.Status, &heartbeat, &installation, &name, &site)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return EdgePair{}, err
		}
		e.Kind, e.Site = "managed", site
		if installation != "" {
			e.Kind, e.ID, e.Name, e.Site = "self_hosted", installation, name, name
		}
		if heartbeat.Valid {
			t := heartbeat.Time.UTC()
			e.LastHeartbeat = &t
		}
		edges, nodeIDs = append(edges, e), append(nodeIDs, id)
	}
	if len(edges) == 0 {
		return EdgePair{}, ErrEdgePairUnassigned
	}
	best, err := machinelatency.BestForAccount(ctx, s.database.SQL(), actor, nodeIDs, now)
	if err != nil {
		return EdgePair{}, err
	}
	for i := range edges {
		if b, ok := best[nodeIDs[i]]; ok {
			rtt, at := b.RTTMillis, b.MeasuredAt
			edges[i].BestRTTMillis, edges[i].MeasuredAt = &rtt, &at
		}
	}
	pair := EdgePair{Primary: edges[0]}
	if len(edges) > 1 {
		pair.Fallback = &edges[1]
	}
	return pair, nil
}

// resolvePairEdge turns an id from GET /v1/edges (a Paperboat node) or a self-hosted tunnel installation id
// into its node. The workspace sees only installations it may use, so an id of another workspace is unknown.
func resolvePairEdge(ctx context.Context, tx *db.Tx, ws edgeset.Scope, id string) (node string, selfHosted bool, err error) {
	err = tx.QueryRow(ctx, `SELECT i.node_id FROM paperboat.selfhost_installations i WHERE i.id=$1 AND i.capability='tunnel' AND i.state='active' AND i.claim_complete AND i.scope_kind<>'global'
 AND ((i.scope_kind=$2 AND i.scope_id=$3) OR ($2='account' AND paperboat.selfhost_scope_visible(i.scope_kind,i.scope_id,$4)))`, id, ws.Kind, ws.ID, ws.Account).Scan(&node)
	if err == nil {
		return node, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", false, err
	}
	err = tx.QueryRow(ctx, `SELECT n.id FROM paperboat.control_tunnel_nodes n WHERE n.id=$1 AND n.roles @> ARRAY['edge']::text[]`, id).Scan(&node)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, fmt.Errorf("%w: %q is not an edge this workspace can use", ErrInvalid, id)
	}
	return node, false, err
}

// SetEdgePair makes primary and fallback the workspace's edge pair through the machinery that already assigns
// edges: the tunnel pool policy (both Paperboat's or both its own servers), the stored edge set that placement
// consults, and the tunnels/previews topics that make its machines look again. Switching between Paperboat's
// edges and its own servers is refused while previews or tunnels run (LivePublicationsError), as the pool is.
func (s *Service) SetEdgePair(ctx context.Context, actor string, scope Scope, primaryID, fallbackID string) (EdgePair, error) {
	for _, id := range []string{primaryID, fallbackID} {
		if id == "" || len(id) > 128 || !utf8.ValidString(id) || strings.ContainsRune(id, 0) {
			return EdgePair{}, fmt.Errorf("%w: primary_id and fallback_id are required", ErrInvalid)
		}
	}
	if primaryID == fallbackID {
		return EdgePair{}, fmt.Errorf("%w: the primary and the fallback must be different edges", ErrInvalid)
	}
	ws, err := s.pairScope(ctx, actor, scope, true)
	if err != nil {
		return EdgePair{}, err
	}
	var nodes []string
	err = s.database.InTx(ctx, func(ctx context.Context, tx *db.Tx) (storeErr error) {
		nodes, storeErr = s.storePair(ctx, tx, actor, ws, primaryID, fallbackID)
		return storeErr
	})
	if err != nil {
		return EdgePair{}, err
	}
	// The pair is stored; a failed wake-up only leaves machines to see it when their attachment next refreshes.
	if err = s.publishPairChange(ctx, ws); err != nil {
		slog.ErrorContext(ctx, "edge_pair_publish_failed", "scope_kind", ws.Kind, "scope_id", ws.ID, "error", err)
	}
	return s.readPair(ctx, actor, ws, nodes)
}

func (s *Service) storePair(ctx context.Context, tx *db.Tx, actor string, ws edgeset.Scope, primaryID, fallbackID string) ([]string, error) {
	now := s.now().UTC()
	// The same lock the pool policy takes, so a pool change and a pair change never interleave.
	if err := edgeset.LockWorkspace(ctx, tx, ws); err != nil {
		return nil, err
	}
	var team any
	if ws.Kind == "team" {
		team = ws.ID
		var allowed bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.teams t JOIN paperboat.team_members m USING(team_id) WHERE t.team_id=$1 AND t.deleted_at IS NULL AND m.account_id=$2 AND m.active
 AND team_role_allows(CASE WHEN t.owner_account=$2 THEN 'owner' ELSE m.role END,'settings.manage'))`, ws.ID, actor).Scan(&allowed); err != nil {
			return nil, err
		}
		if !allowed {
			return nil, ErrUnauthorized
		}
	}
	first, firstOwn, err := resolvePairEdge(ctx, tx, ws, primaryID)
	if err != nil {
		return nil, err
	}
	second, secondOwn, err := resolvePairEdge(ctx, tx, ws, fallbackID)
	if err != nil {
		return nil, err
	}
	if first == second {
		return nil, fmt.Errorf("%w: the primary and the fallback must be different edges", ErrInvalid)
	}
	if firstOwn != secondOwn {
		return nil, fmt.Errorf("%w: both edges must be Paperboat's or both your own servers, never one of each", ErrInvalid)
	}
	mode, installations := "managed-only", []string{}
	if firstOwn {
		mode = "self-hosted-only"
		installations = append(installations, primaryID, fallbackID)
	}
	var current string
	if err = tx.QueryRow(ctx, `SELECT paperboat.workspace_edge_mode($1,$2)`, ws.Account, team).Scan(&current); err != nil {
		return nil, err
	}
	if current != mode {
		var live int
		if err = tx.QueryRow(ctx, liveEdgePublicationsSQL, ws.Account, team).Scan(&live); err != nil {
			return nil, err
		}
		if live > 0 {
			return nil, &LivePublicationsError{Count: live}
		}
	}
	// The pool is the pair: the policy decides which edges may serve the workspace, the edge set which of them do.
	if _, err = tx.Exec(ctx, `INSERT INTO paperboat.selfhost_pool_policies(scope_kind,scope_id,capability,mode,installation_ids) VALUES($1,$2,'tunnel',$3,$4::text[])
ON CONFLICT(scope_kind,scope_id,capability) DO UPDATE SET mode=excluded.mode,installation_ids=excluded.installation_ids`, ws.Kind, ws.ID, mode, installations); err != nil {
		return nil, err
	}
	// Eligibility is judged under the new mode, and an edge nobody has heard from lately would be replaced by the
	// next reconcile, silently undoing the choice.
	for i, node := range []string{first, second} {
		var ok bool
		if err = tx.QueryRow(ctx, `SELECT paperboat.node_workspace_edge_eligible($1,$2,$3) AND EXISTS(SELECT 1 FROM paperboat.control_tunnel_nodes n WHERE n.id=$1 AND n.roles @> ARRAY['edge']::text[] AND n.last_heartbeat_at>$4::timestamptz-make_interval(secs=>$5::float8))`,
			node, ws.Account, team, now, edgeset.ReplaceAfter.Seconds()).Scan(&ok); err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("%w: %q is not available to this workspace right now; choose an edge that is ready", ErrInvalid, []string{primaryID, fallbackID}[i])
		}
	}
	nodes := []string{first, second}
	if err = edgeset.Store(ctx, tx, ws, mode, nodes, now); err != nil {
		return nil, err
	}
	return nodes, nil
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
