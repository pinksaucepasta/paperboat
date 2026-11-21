// Package operatorinfra manages the infrastructure registry through the operator API: regions and the
// relay and edge nodes in them, with paged listing and idempotent, audited mutations. It also resolves
// a runtime node from its credential and issues a node's runtime credential, which it keeps encrypted
// under the configured key.
package operatorinfra

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pinksaucepasta/paperboat/server/internal/audit"
	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/edgeset"
	"github.com/pinksaucepasta/paperboat/server/internal/nodedrain"
	"github.com/pinksaucepasta/paperboat/server/internal/selfhost"
	"github.com/pinksaucepasta/paperboat/shared/nodemesh"
)

var (
	ErrInvalid   = errors.New("invalid infrastructure operation")
	ErrForbidden = errors.New("infrastructure operation forbidden")
	ErrNotFound  = errors.New("infrastructure resource not found")
	ErrConflict  = errors.New("infrastructure operation conflict")
	// ErrLastServingEdge refuses to take an enabled region's last serving edge out (drain, retire, or an update that
	// re-registers it): the region would leave the wildcard DNS names and every pair using it.
	ErrLastServingEdge = errors.New("last serving edge of an enabled region")
)

type Service struct {
	store *db.DB
	audit *audit.Writer
	key   string
	// probeIngress is selfhost.ProbePublicIngress; tests replace it.
	probeIngress   func(ctx context.Context, host, node, credential string) (any, any, error)
	ingressMu      sync.Mutex
	ingressRetries map[string]ingressRetry
}

func NewService(store *db.DB, encryptionKey string) *Service {
	return &Service{store: store, audit: audit.NewWriter(store), key: encryptionKey, probeIngress: selfhost.ProbePublicIngress}
}

type Region struct {
	ID          string    `json:"id"`
	DisplayName string    `json:"display_name"`
	Enabled     bool      `json:"enabled"`
	Revision    int64     `json:"revision"`
	UpdatedAt   time.Time `json:"updated_at"`
}
type RegionMutation struct {
	Action      string `json:"action"`
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Reason      string `json:"reason"`
	Revision    int64  `json:"revision"`
}
type RegionPage struct {
	Total     int64    `json:"total"`
	Items     []Region `json:"items"`
	NextAfter string   `json:"next_after,omitempty"`
}
type Node struct {
	NodeGeneration    int64      `json:"node_generation"`
	ObservedAt        *time.Time `json:"observed_at,omitempty"`
	RegistryExpiresAt time.Time  `json:"registry_expires_at"`
	ID                string     `json:"id"`
	ProtocolVersion   string     `json:"protocol_version"`
	RegionID          string     `json:"region_id"`
	EdgePool          string     `json:"edge_pool"`
	FailureDomain     string     `json:"failure_domain"`
	State             string     `json:"state"`
	EndpointHost      string     `json:"endpoint_host"`
	EndpointTCPPort   int32      `json:"endpoint_tcp_port"`
	EndpointQUICPort  int32      `json:"endpoint_quic_port"`
	Roles             []string   `json:"roles"`
	Transports        []string   `json:"transports"`
	CapacityLimit     int64      `json:"capacity_limit"`
	Revision          int64      `json:"revision"`
	Ready             bool       `json:"ready"`
	// Carriers counts the machines holding a live carrier attachment on the node; 0 means safe to retire.
	Carriers int64 `json:"carriers"`
	// LoadPercent is the node's load (node_load_percent: the worst of sustained CPU and connections against its own
	// limit); absent when it reported nothing in the last 2 minutes.
	LoadPercent *float64 `json:"load_percent,omitempty"`
	// InRotation is false while the load rule holds the ready node out: an edge is out of the wildcard DNS answers, a
	// relay out of the relay candidate list; absent for nodes the rule does not manage.
	InRotation *bool `json:"in_rotation,omitempty"`
	// IngressVerified: the node's public ingress addresses passed the operator's verify-ingress probe.
	IngressVerified bool `json:"ingress_verified"`
	// Forward is the node's forwarding counters from its latest observation: an edge's visitor forwarding, a relay's mesh.
	Forward *nodemesh.Stats `json:"forward,omitempty"`
}
type NodeMutation struct {
	Action   string `json:"action"`
	NodeID   string `json:"node_id"`
	Reason   string `json:"reason"`
	Revision int64  `json:"revision"`
	Site     string `json:"site,omitempty"` // city or site shown next to the region; only the site action takes it
}
type NodeMutationResult struct {
	Node Node `json:"node"`
}
type NodePage struct {
	Total     int64  `json:"total"`
	Items     []Node `json:"items"`
	NextAfter string `json:"next_after,omitempty"`
}

func (s *Service) MutateRegion(ctx context.Context, actor, key string, in RegionMutation) (Region, error) {
	var out Region
	in.Action = strings.TrimSpace(in.Action)
	in.ID = strings.TrimSpace(in.ID)
	in.DisplayName = strings.TrimSpace(in.DisplayName)
	if !validCommon(actor, key, in.ID, in.Reason) || !regionIdentifier.MatchString(in.ID) || len(in.DisplayName) > 80 || in.Revision < 0 || (in.Action != "create" && in.Action != "update" && in.Action != "enable" && in.Action != "disable") {
		return out, ErrInvalid
	}
	err := s.mutate(ctx, actor, key, "region."+in.Action, in.ID, in, &out, func(ctx context.Context, tx *db.Tx) error {
		if in.Action == "create" {
			if in.Revision != 0 || in.DisplayName == "" {
				return ErrInvalid
			}
			err := tx.QueryRow(ctx, `INSERT INTO operator_regions(id,display_name) VALUES($1,$2) RETURNING id,display_name,enabled,revision,updated_at`, in.ID, in.DisplayName).Scan(&out.ID, &out.DisplayName, &out.Enabled, &out.Revision, &out.UpdatedAt)
			if err != nil {
				return mapErr(err)
			}
			return nil
		}
		var enabled bool
		var revision int64
		var name string
		if err := tx.QueryRow(ctx, `SELECT display_name,enabled,revision FROM operator_regions WHERE id=$1 FOR UPDATE`, in.ID).Scan(&name, &enabled, &revision); err == pgx.ErrNoRows {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		if revision != in.Revision {
			return ErrConflict
		}
		switch in.Action {
		case "update":
			if in.DisplayName == "" {
				return ErrInvalid
			}
			name = in.DisplayName
		case "enable":
			enabled = true
		case "disable":
			enabled = false
		}
		if err := tx.QueryRow(ctx, `UPDATE operator_regions SET display_name=$2,enabled=$3,revision=revision+1,updated_at=now() WHERE id=$1 RETURNING id,display_name,enabled,revision,updated_at`, in.ID, name, enabled).Scan(&out.ID, &out.DisplayName, &out.Enabled, &out.Revision, &out.UpdatedAt); err != nil {
			return err
		}
		if in.Action == "disable" {
			_, err := tx.Exec(ctx, `UPDATE control_tunnel_nodes n SET state='draining',ready=false,drain_deadline=coalesce(drain_deadline,now()+interval '30 seconds'),version=version+1,updated_at=now() FROM managed_node_authority m WHERE m.node_id=n.id AND m.region_id=$1 AND m.retired_at IS NULL AND n.state<>'retired'`, in.ID)
			return err
		}
		return nil
	})
	if err != nil {
		return Region{}, err
	}
	return out, nil
}

func (s *Service) MutateNode(ctx context.Context, actor, key string, in NodeMutation) (NodeMutationResult, error) {
	var out NodeMutationResult
	if !validCommon(actor, key, in.NodeID, in.Reason) || in.Revision < 0 {
		return out, ErrInvalid
	}
	err := s.mutate(ctx, actor, key, "node."+in.Action, in.NodeID, in, &out, func(ctx context.Context, tx *db.Tx) error {
		switch in.Action {
		case "verify-ingress":
			if in.Revision < 1 {
				return ErrInvalid
			}
			if err := s.verifyNodeIngress(ctx, tx, in.NodeID, in.Revision); err != nil {
				return err
			}
		case "site":
			// Any non-retired node, managed or a workspace's own.
			if !validSite(in.Site) {
				return ErrInvalid
			}
			tag, err := tx.Exec(ctx, `UPDATE control_tunnel_nodes SET site=$2 WHERE id=$1 AND state<>'retired'`, in.NodeID, in.Site)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return ErrNotFound
			}
		case "drain", "drain-move", "resume", "retire":
			if in.Revision < 1 {
				return ErrInvalid
			}
			var regionEnabled bool
			err := tx.QueryRow(ctx, `SELECT r.enabled FROM managed_node_authority m JOIN operator_regions r ON r.id=m.region_id WHERE m.node_id=$1 AND m.revision=$2 AND m.retired_at IS NULL FOR UPDATE OF r`, in.NodeID, in.Revision).Scan(&regionEnabled)
			if err == pgx.ErrNoRows {
				return ErrConflict
			} else if err != nil {
				return err
			}
			if in.Action == "resume" && !regionEnabled {
				return ErrConflict
			}
			if in.Action != "resume" {
				if err = refuseLastServingEdge(ctx, tx, in.NodeID); err != nil {
					return err
				}
			}
			switch in.Action {
			case "drain", "drain-move":
				err = nodedrain.Begin(ctx, tx, in.NodeID, in.Action == "drain-move")
			case "resume":
				err = nodedrain.Resume(ctx, tx, in.NodeID)
			default:
				_, err = tx.Exec(ctx, `UPDATE control_tunnel_nodes SET state='retired',ready=false,drain_deadline=NULL,version=version+1,updated_at=now() WHERE id=$1`, in.NodeID)
			}
			if errors.Is(err, nodedrain.ErrNotFound) || errors.Is(err, nodedrain.ErrRetired) {
				return ErrConflict
			}
			if err != nil {
				return err
			}
		default:
			return ErrInvalid
		}
		return readNodeTx(ctx, tx, in.NodeID, &out.Node)
	})
	if err != nil {
		return NodeMutationResult{}, err
	}
	if in.Action == "drain-move" {
		// A failed wake is not an error: the drain deadline still ends the carriers, only later.
		if err := nodedrain.Wake(ctx, s.store.Pool(), in.NodeID); err != nil {
			slog.WarnContext(ctx, "operator_drain_move_wake_failed", "node_id", in.NodeID, "error", err)
		}
	}
	return out, nil
}

// refuseLastServingEdge returns ErrLastServingEdge when node is the last serving edge of its enabled region. It locks
// the region row, so every drain, retire and update in one region is serialized and two operators cannot each take out
// one of its last two serving edges. Bring a replacement up first, or disable the region.
func refuseLastServingEdge(ctx context.Context, tx *db.Tx, node string) error {
	var last bool
	err := tx.QueryRow(ctx, `SELECT r.enabled AND $1 IN (SELECT id FROM `+edgeset.ServingNodes+` s)
 AND NOT EXISTS(SELECT 1 FROM `+edgeset.ServingNodes+` s JOIN managed_node_authority o ON o.node_id=s.id WHERE o.region_id=r.id AND s.id<>$1)
 FROM managed_node_authority m JOIN operator_regions r ON r.id=m.region_id WHERE m.node_id=$1 FOR UPDATE OF r`, node).Scan(&last)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // not a managed node: the revision checks that follow refuse it
	}
	if err != nil {
		return err
	}
	if last {
		return ErrLastServingEdge
	}
	return nil
}

func (s *Service) ListRegions(ctx context.Context, actor, after string, limit int) (RegionPage, error) {
	var out RegionPage
	if limit < 1 || limit > 100 || (after != "" && !identifier.MatchString(after)) {
		return out, ErrInvalid
	}
	err := s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		out = RegionPage{Items: []Region{}}
		if err := allowRead(ctx, tx, actor); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM operator_regions`).Scan(&out.Total); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id,display_name,enabled,revision,updated_at FROM operator_regions WHERE id>$1 ORDER BY id LIMIT $2`, after, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v Region
			if err = rows.Scan(&v.ID, &v.DisplayName, &v.Enabled, &v.Revision, &v.UpdatedAt); err != nil {
				rows.Close()
				return errors.Join(err, rows.Err())
			}
			if len(out.Items) == limit {
				out.NextAfter = out.Items[len(out.Items)-1].ID
				break
			}
			out.Items = append(out.Items, v)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: actor, ActorType: audit.ActorAdmin, EventType: "operator.infrastructure.regions_listed", ResourceType: "infrastructure_region", ResourceID: "*"})
	})
	if err != nil {
		return RegionPage{}, err
	}
	return out, nil
}
func (s *Service) ListNodes(ctx context.Context, actor, region, after string, limit int) (NodePage, error) {
	var out NodePage
	if region != "" && !regionIdentifier.MatchString(region) {
		return out, ErrInvalid
	}
	if limit < 1 || limit > 100 || (after != "" && !identifier.MatchString(after)) {
		return out, ErrInvalid
	}
	err := s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		out = NodePage{Items: []Node{}}
		if err := allowRead(ctx, tx, actor); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM control_tunnel_nodes n JOIN managed_node_authority m ON m.node_id=n.id WHERE ($1='' OR m.region_id=$1)`, region).Scan(&out.Total); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT n.id,n.protocol_version,coalesce(m.region_id,n.region,''),n.edge_pool,n.failure_domain,n.state,n.endpoint_host,n.endpoint_tcp_port,n.endpoint_quic_port,n.roles,n.transports,n.capacity_limit,m.revision,n.node_generation,n.last_heartbeat_at,n.registry_expires_at,(n.ready AND n.registry_expires_at>now() AND n.last_heartbeat_at>$4 AND r.enabled AND m.retired_at IS NULL),(SELECT count(DISTINCT a.machine_id) FROM runtime_carrier_attachments a WHERE a.edge_node_id=n.id AND a.expires_at>now()),node_load_percent(n.id)::float8,(SELECT NOT w.out_for_load FROM node_load_out w WHERE w.node_id=n.id),n.public_ingress_verified_at IS NOT NULL,coalesce(n.observation->'forward',n.observation->'mesh') FROM control_tunnel_nodes n JOIN managed_node_authority m ON m.node_id=n.id JOIN operator_regions r ON r.id=m.region_id WHERE ($1='' OR m.region_id=$1) AND n.id>$2 ORDER BY n.id LIMIT $3`, region, after, limit+1, time.Now().Add(-controlplane.ControlTunnelNodeStaleAfter()))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v Node
			var forward []byte
			if err = rows.Scan(&v.ID, &v.ProtocolVersion, &v.RegionID, &v.EdgePool, &v.FailureDomain, &v.State, &v.EndpointHost, &v.EndpointTCPPort, &v.EndpointQUICPort, &v.Roles, &v.Transports, &v.CapacityLimit, &v.Revision, &v.NodeGeneration, &v.ObservedAt, &v.RegistryExpiresAt, &v.Ready, &v.Carriers, &v.LoadPercent, &v.InRotation, &v.IngressVerified, &forward); err != nil {
				rows.Close()
				return errors.Join(err, rows.Err())
			}
			v.Forward = forwardStats(forward)
			if len(out.Items) == limit {
				out.NextAfter = out.Items[len(out.Items)-1].ID
				break
			}
			out.Items = append(out.Items, v)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: actor, ActorType: audit.ActorAdmin, EventType: "operator.infrastructure.nodes_listed", ResourceType: "infrastructure_node", ResourceID: region})
	})
	if err != nil {
		return NodePage{}, err
	}
	return out, nil
}
func allowRead(ctx context.Context, tx *db.Tx, actor string) error {
	var role, status string
	err := tx.QueryRow(ctx, `SELECT role,status FROM operator_accounts WHERE id=$1 FOR SHARE`, actor).Scan(&role, &status)
	if err == pgx.ErrNoRows {
		return ErrForbidden
	}
	if err != nil {
		return err
	}
	if status != "active" || (role != "admin" && role != "operations") {
		return ErrForbidden
	}
	return nil
}

func (s *Service) mutate(ctx context.Context, actor, key, kind, target string, request, resultValue any, fn func(context.Context, *db.Tx) error) error {
	raw, _ := json.Marshal(request)
	hash := sha256.Sum256(raw)
	reason := ""
	switch value := request.(type) {
	case RegionMutation:
		reason = value.Reason
	case NodeMutation:
		reason = value.Reason
	}
	return s.store.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		switch result := resultValue.(type) {
		case *NodeMutationResult:
			*result = NodeMutationResult{}
		case *Region:
			*result = Region{}
		}
		lockHash := sha256.Sum256([]byte(actor + "\x00" + key))
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(encode($1,'hex'),218))`, lockHash[:]); err != nil {
			return err
		}
		var role, status string
		err := tx.QueryRow(ctx, `SELECT role,status FROM operator_accounts WHERE id=$1 FOR SHARE`, actor).Scan(&role, &status)
		if err == pgx.ErrNoRows {
			return ErrForbidden
		}
		if err != nil {
			return err
		}
		if status != "active" || role != "admin" {
			return ErrForbidden
		}
		var old, result []byte
		err = tx.QueryRow(ctx, `SELECT request_hash,result FROM operator_infrastructure_operations WHERE actor_user_id=$1 AND idempotency_key=$2`, actor, key).Scan(&old, &result)
		if err == nil {
			if string(old) != string(hash[:]) {
				return ErrConflict
			}
			return json.Unmarshal(result, resultValue)
		}
		if err != pgx.ErrNoRows {
			return err
		}
		if err = fn(ctx, tx); err != nil {
			return err
		}
		result, _ = json.Marshal(resultValue)
		if _, err = tx.Exec(ctx, `INSERT INTO operator_infrastructure_operations(actor_user_id,idempotency_key,request_hash,result) VALUES($1,$2,$3,$4)`, actor, key, hash[:], result); err != nil {
			var pgerr *pgconn.PgError
			if errors.As(err, &pgerr) && pgerr.Code == "23505" && pgerr.ConstraintName == "operator_infrastructure_operations_pkey" {
				return &pgconn.PgError{Code: "40001", Message: "concurrent infrastructure operation receipt"}
			}
			return err
		}
		return s.audit.WriteTx(ctx, tx, audit.Event{ActorUserID: actor, ActorType: audit.ActorAdmin, EventType: "operator.infrastructure." + kind, ResourceType: "infrastructure", ResourceID: target, IdempotencyKey: key, Metadata: map[string]any{"reason": reason}})
	})
}

func readNodeTx(ctx context.Context, tx *db.Tx, id string, out *Node) error {
	var forward []byte
	err := tx.QueryRow(ctx, `SELECT n.id,n.protocol_version,coalesce(m.region_id,n.region,''),n.edge_pool,n.failure_domain,n.state,n.endpoint_host,n.endpoint_tcp_port,n.endpoint_quic_port,n.roles,n.transports,n.capacity_limit,coalesce(m.revision,0),n.node_generation,n.last_heartbeat_at,n.registry_expires_at,(n.ready AND n.registry_expires_at>now() AND n.last_heartbeat_at>$2 AND coalesce(r.enabled,true) AND m.retired_at IS NULL),(SELECT count(DISTINCT a.machine_id) FROM runtime_carrier_attachments a WHERE a.edge_node_id=n.id AND a.expires_at>now()),node_load_percent(n.id)::float8,(SELECT NOT w.out_for_load FROM node_load_out w WHERE w.node_id=n.id),n.public_ingress_verified_at IS NOT NULL,coalesce(n.observation->'forward',n.observation->'mesh') FROM control_tunnel_nodes n LEFT JOIN managed_node_authority m ON m.node_id=n.id LEFT JOIN operator_regions r ON r.id=m.region_id WHERE n.id=$1`, id, time.Now().Add(-controlplane.ControlTunnelNodeStaleAfter())).Scan(&out.ID, &out.ProtocolVersion, &out.RegionID, &out.EdgePool, &out.FailureDomain, &out.State, &out.EndpointHost, &out.EndpointTCPPort, &out.EndpointQUICPort, &out.Roles, &out.Transports, &out.CapacityLimit, &out.Revision, &out.NodeGeneration, &out.ObservedAt, &out.RegistryExpiresAt, &out.Ready, &out.Carriers, &out.LoadPercent, &out.InRotation, &out.IngressVerified, &forward)
	out.Forward = forwardStats(forward)
	return err
}

// forwardStats reads the stored forwarding counters; an absent or unreadable value is no stats, never a failed read.
func forwardStats(raw []byte) *nodemesh.Stats {
	var stats nodemesh.Stats
	if len(raw) == 0 || json.Unmarshal(raw, &stats) != nil {
		return nil
	}
	return &stats
}

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)
var regionIdentifier = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)
var endpointName = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)

// validSite: a short display name (a city), printable, no surrounding space.
func validSite(v string) bool {
	return len(v) <= 80 && strings.TrimSpace(v) == v && v != "" && utf8.ValidString(v) && !strings.ContainsFunc(v, unicode.IsControl)
}

func validCommon(actor, key, target, reason string) bool {
	return identifier.MatchString(actor) && len(key) >= 8 && len(key) <= 200 && identifier.MatchString(key) && identifier.MatchString(target) && identifier.MatchString(reason)
}

func mapErr(err error) error {
	var p *pgconn.PgError
	if errors.As(err, &p) {
		switch p.Code {
		case "23505":
			return ErrConflict
		case "23514", "22003", "22P02":
			return ErrInvalid
		}
	}
	return err
}
