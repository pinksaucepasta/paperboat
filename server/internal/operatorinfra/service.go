package operatorinfra

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/shared/relaylimits"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pinksaucepasta/paperboat-server/internal/audit"
	"github.com/pinksaucepasta/paperboat-server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/nodeauth"
	"github.com/pinksaucepasta/paperboat-server/internal/secrets"
	"github.com/pinksaucepasta/paperboat-server/internal/selfhost"
)

var (
	ErrInvalid   = errors.New("invalid infrastructure operation")
	ErrForbidden = errors.New("infrastructure operation forbidden")
	ErrNotFound  = errors.New("infrastructure resource not found")
	ErrConflict  = errors.New("infrastructure operation conflict")
)

type Service struct {
	store *db.DB
	audit *audit.Writer
	key   string
}

func NewService(store *db.DB, encryptionKey string) *Service {
	return &Service{store: store, audit: audit.NewWriter(store), key: encryptionKey}
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
	Items     []Region `json:"items"`
	NextAfter string   `json:"next_after,omitempty"`
}
type Node struct {
	NodeGeneration    int64      `json:"node_generation"`
	ObservedAt        *time.Time `json:"observed_at,omitempty"`
	RegistryExpiresAt time.Time  `json:"registry_expires_at"`
	ID                string     `json:"id"`
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
}
type NodeMutation struct {
	Action                 string   `json:"action"`
	NodeID                 string   `json:"node_id"`
	RegionID               string   `json:"region_id"`
	EdgePool               string   `json:"edge_pool"`
	FailureDomain          string   `json:"failure_domain"`
	EndpointHost           string   `json:"endpoint_host"`
	Reason                 string   `json:"reason"`
	EndpointTCPPort        int32    `json:"endpoint_tcp_port"`
	EndpointQUICPort       int32    `json:"endpoint_quic_port"`
	Roles                  []string `json:"roles"`
	Transports             []string `json:"transports"`
	CapacityLimit          int64    `json:"capacity_limit"`
	Revision               int64    `json:"revision"`
	UsagePublicKey         []byte   `json:"usage_public_key,omitempty"`
	PeerWireGuardPublicKey []byte   `json:"peer_wireguard_public_key,omitempty"`
	PeerDiscoPublicKey     []byte   `json:"peer_disco_public_key,omitempty"`
	PeerVirtualAddress     string   `json:"peer_virtual_address,omitempty"`
}
type NodeMutationResult struct {
	Node              Node   `json:"node"`
	RuntimeCredential string `json:"runtime_credential,omitempty"`
}
type NodePage struct {
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
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE managed_node_authority SET revision=revision+1,updated_at=now() WHERE region_id=$1 AND retired_at IS NULL`, in.ID)
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
			var host, epoch string
			var cipher, runtimeHash []byte
			err := tx.QueryRow(ctx, `SELECT n.endpoint_host,n.process_epoch,m.runtime_ciphertext,m.runtime_hash FROM control_tunnel_nodes n JOIN managed_node_authority m ON m.node_id=n.id JOIN operator_regions r ON r.id=m.region_id WHERE n.id=$1 AND m.revision=$2 AND m.retired_at IS NULL AND r.enabled AND n.state='ready' AND n.ready AND n.last_heartbeat_at>now()-interval '15 seconds' AND n.registry_expires_at>now() AND 'edge'=ANY(n.roles)`, in.NodeID, in.Revision).Scan(&host, &epoch, &cipher, &runtimeHash)
			if err == pgx.ErrNoRows {
				return ErrConflict
			}
			if err != nil {
				return err
			}
			credential, err := secrets.Decrypt(s.key, cipher)
			if err != nil {
				return err
			}
			check := sha256.Sum256([]byte(credential))
			if !bytes.Equal(check[:], runtimeHash) {
				return ErrConflict
			}
			probeCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
			defer cancel()
			v4, v6, err := selfhost.ProbePublicIngress(probeCtx, host, in.NodeID, credential)
			if err != nil {
				return ErrConflict
			}
			tag, err := tx.Exec(ctx, `UPDATE control_tunnel_nodes n SET public_ingress_ipv4=$3::inet,public_ingress_ipv6=$4::inet,public_ingress_verified_at=now() WHERE n.id=$1 AND n.process_epoch=$2 AND n.endpoint_host=$5 AND n.state='ready' AND n.ready AND n.last_heartbeat_at>now()-interval '15 seconds' AND n.registry_expires_at>now() AND 'edge'=ANY(n.roles) AND EXISTS (SELECT 1 FROM managed_node_authority m JOIN operator_regions r ON r.id=m.region_id WHERE m.node_id=n.id AND m.revision=$6 AND m.runtime_hash=$7 AND m.retired_at IS NULL AND r.enabled)`, in.NodeID, epoch, v4, v6, host, in.Revision, runtimeHash)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return ErrConflict
			}
		case "enroll":
			if in.Revision != 0 || !validNode(in) || len(s.key) < 32 {
				return ErrInvalid
			}
			var enabled bool
			if err := tx.QueryRow(ctx, `SELECT enabled FROM operator_regions WHERE id=$1`, in.RegionID).Scan(&enabled); err == pgx.ErrNoRows {
				return ErrConflict
			} else if err != nil {
				return err
			}
			if !enabled {
				return ErrConflict
			}
			credential := randomToken()
			hash := sha256.Sum256([]byte(credential))
			cipher, err := secrets.Encrypt(s.key, credential)
			if err != nil {
				return err
			}
			config, _ := json.Marshal(publicConfig(in))
			now := time.Now().UTC()
			_, err = tx.Exec(ctx, `INSERT INTO control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,endpoint_host,endpoint_tcp_port,endpoint_quic_port,carrier_endpoint_host,carrier_endpoint_tcp_port,carrier_endpoint_quic_port,state,ready,last_heartbeat_at,node_generation,region,failure_domain,roles,transports,capacity_limit,capacity_used,capacity_observed_at,registry_expires_at,peer_relay_wireguard_public_key,peer_relay_disco_public_key,peer_relay_virtual_address) VALUES($1,$2,'1.0','',$3,$4,$5,$3,$4,$5,'registered',false,NULL,1,$7,$8,$9,$10,$11,0,$6,$6,$12,$13,nullif($14,'')::inet)`, in.NodeID, in.EdgePool, in.EndpointHost, in.EndpointTCPPort, in.EndpointQUICPort, now, in.RegionID, in.FailureDomain, in.Roles, in.Transports, in.CapacityLimit, nullableBytes(in.PeerWireGuardPublicKey), nullableBytes(in.PeerDiscoPublicKey), in.PeerVirtualAddress)
			if err != nil {
				return mapErr(err)
			}
			_, err = tx.Exec(ctx, `INSERT INTO managed_node_authority(node_id,region_id,runtime_hash,runtime_ciphertext,config) VALUES($1,$2,$3,$4,$5)`, in.NodeID, in.RegionID, hash[:], cipher, config)
			if err != nil {
				return mapErr(err)
			}
			if len(in.UsagePublicKey) == 32 {
				_, err = tx.Exec(ctx, `INSERT INTO control_usage_verification_keys(key_id,edge_node_id,public_key,not_before,expires_at) VALUES($1,$2,$3,$4,$5)`, "managed:"+in.NodeID, in.NodeID, in.UsagePublicKey, now, now.AddDate(10, 0, 0))
				if err != nil {
					return err
				}
			}
			out.RuntimeCredential = credential
		case "rotate":
			if in.Revision < 1 {
				return ErrInvalid
			}
			credential := randomToken()
			hash := sha256.Sum256([]byte(credential))
			cipher, err := secrets.Encrypt(s.key, credential)
			if err != nil {
				return err
			}
			tag, err := tx.Exec(ctx, `UPDATE managed_node_authority SET runtime_hash=$2,runtime_ciphertext=$3,revision=revision+1,updated_at=now() WHERE node_id=$1 AND revision=$4 AND retired_at IS NULL`, in.NodeID, hash[:], cipher, in.Revision)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return ErrConflict
			}
			out.RuntimeCredential = credential
			_, err = tx.Exec(ctx, `UPDATE control_tunnel_nodes SET public_ingress_ipv4=NULL,public_ingress_ipv6=NULL,public_ingress_verified_at=NULL WHERE id=$1`, in.NodeID)
			if err != nil {
				return err
			}
		case "update":
			if in.Revision < 1 || !validNode(in) {
				return ErrInvalid
			}
			var previousRaw []byte
			if err := tx.QueryRow(ctx, `SELECT config FROM managed_node_authority WHERE node_id=$1 AND revision=$2 AND retired_at IS NULL FOR UPDATE`, in.NodeID, in.Revision).Scan(&previousRaw); err == pgx.ErrNoRows {
				return ErrConflict
			} else if err != nil {
				return err
			}
			var previous nodeConfig
			if err := json.Unmarshal(previousRaw, &previous); err != nil {
				return err
			}
			if !bytes.Equal(previous.UsagePublicKey, in.UsagePublicKey) || !bytes.Equal(previous.PeerWireGuardPublicKey, in.PeerWireGuardPublicKey) || !bytes.Equal(previous.PeerDiscoPublicKey, in.PeerDiscoPublicKey) || previous.PeerVirtualAddress != in.PeerVirtualAddress {
				return ErrConflict
			}
			var enabled bool
			if err := tx.QueryRow(ctx, `SELECT enabled FROM operator_regions WHERE id=$1`, in.RegionID).Scan(&enabled); err == pgx.ErrNoRows {
				return ErrConflict
			} else if err != nil {
				return err
			}
			if !enabled {
				return ErrConflict
			}
			config, _ := json.Marshal(publicConfig(in))
			tag, err := tx.Exec(ctx, `UPDATE managed_node_authority SET region_id=$2,config=$3,revision=revision+1,updated_at=now() WHERE node_id=$1 AND revision=$4 AND retired_at IS NULL`, in.NodeID, in.RegionID, config, in.Revision)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return ErrConflict
			}
			_, err = tx.Exec(ctx, `UPDATE control_tunnel_nodes SET edge_pool=$2,endpoint_host=$3,endpoint_tcp_port=$4,endpoint_quic_port=$5,carrier_endpoint_host=$3,carrier_endpoint_tcp_port=$4,carrier_endpoint_quic_port=$5,region=$6,failure_domain=$7,roles=$8,transports=$9,capacity_limit=$10,capacity_used=0,capacity_observed_at=now(),state='registered',ready=false,node_generation=node_generation+1,process_epoch='',registry_expires_at=now(),drain_deadline=NULL,public_ingress_ipv4=NULL,public_ingress_ipv6=NULL,public_ingress_verified_at=NULL,peer_relay_wireguard_public_key=$11,peer_relay_disco_public_key=$12,peer_relay_virtual_address=nullif($13,'')::inet,version=version+1,updated_at=now() WHERE id=$1 AND state<>'retired'`, in.NodeID, in.EdgePool, in.EndpointHost, in.EndpointTCPPort, in.EndpointQUICPort, in.RegionID, in.FailureDomain, in.Roles, in.Transports, in.CapacityLimit, nullableBytes(in.PeerWireGuardPublicKey), nullableBytes(in.PeerDiscoPublicKey), in.PeerVirtualAddress)
			if err != nil {
				return err
			}
		case "drain", "resume", "retire":
			if in.Revision < 1 {
				return ErrInvalid
			}
			state := "draining"
			if in.Action == "resume" {
				state = "registered"
			}
			if in.Action == "retire" {
				state = "retired"
			}
			var regionEnabled bool
			err := tx.QueryRow(ctx, `SELECT r.enabled FROM managed_node_authority m JOIN operator_regions r ON r.id=m.region_id WHERE m.node_id=$1 AND m.revision=$2 AND m.retired_at IS NULL FOR UPDATE OF m`, in.NodeID, in.Revision).Scan(&regionEnabled)
			if err == pgx.ErrNoRows {
				return ErrConflict
			} else if err != nil {
				return err
			}
			if in.Action == "resume" && !regionEnabled {
				return ErrConflict
			}
			_, err = tx.Exec(ctx, `UPDATE control_tunnel_nodes SET state=$2,ready=false,drain_deadline=CASE WHEN $2='draining' THEN now()+interval '30 seconds' ELSE NULL END,version=version+1,updated_at=now() WHERE id=$1`, in.NodeID, state)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `UPDATE managed_node_authority SET revision=revision+1,retired_at=CASE WHEN $2='retired' THEN now() ELSE retired_at END,updated_at=now() WHERE node_id=$1`, in.NodeID, state)
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
	return out, nil
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
		rows, err := tx.Query(ctx, `SELECT n.id,m.region_id,n.edge_pool,n.failure_domain,n.state,n.endpoint_host,n.endpoint_tcp_port,n.endpoint_quic_port,n.roles,n.transports,n.capacity_limit,m.revision,n.node_generation,n.last_heartbeat_at,n.registry_expires_at,(n.ready AND n.registry_expires_at>now() AND n.last_heartbeat_at>$4 AND r.enabled AND m.retired_at IS NULL) FROM control_tunnel_nodes n JOIN managed_node_authority m ON m.node_id=n.id JOIN operator_regions r ON r.id=m.region_id WHERE ($1='' OR m.region_id=$1) AND n.id>$2 ORDER BY n.id LIMIT $3`, region, after, limit+1, time.Now().Add(-controlplane.ControlTunnelNodeStaleAfter()))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v Node
			if err = rows.Scan(&v.ID, &v.RegionID, &v.EdgePool, &v.FailureDomain, &v.State, &v.EndpointHost, &v.EndpointTCPPort, &v.EndpointQUICPort, &v.Roles, &v.Transports, &v.CapacityLimit, &v.Revision, &v.NodeGeneration, &v.ObservedAt, &v.RegistryExpiresAt, &v.Ready); err != nil {
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
			if node, ok := resultValue.(*NodeMutationResult); ok {
				var stored nodeReceipt
				if err := json.Unmarshal(result, &stored); err != nil {
					return err
				}
				node.Node = stored.Node
				if len(stored.CredentialCiphertext) > 0 {
					value, err := secrets.Decrypt(s.key, stored.CredentialCiphertext)
					if err != nil {
						return err
					}
					node.RuntimeCredential = value
				}
				return nil
			}
			return json.Unmarshal(result, resultValue)
		}
		if err != pgx.ErrNoRows {
			return err
		}
		if err = fn(ctx, tx); err != nil {
			return err
		}
		if node, ok := resultValue.(*NodeMutationResult); ok {
			var cipher []byte
			if node.RuntimeCredential != "" {
				var err error
				cipher, err = secrets.Encrypt(s.key, node.RuntimeCredential)
				if err != nil {
					return err
				}
			}
			result, _ = json.Marshal(nodeReceipt{Node: node.Node, CredentialCiphertext: cipher})
		} else {
			result, _ = json.Marshal(resultValue)
		}
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

type nodeReceipt struct {
	Node                 Node   `json:"node"`
	CredentialCiphertext []byte `json:"credential_ciphertext,omitempty"`
}

func (s *Service) RuntimeNode(ctx context.Context, credential string) (string, error) {
	if len(credential) != 43 {
		return "", nodeauth.ErrNotFound
	}
	h := sha256.Sum256([]byte(credential))
	var node string
	err := s.store.SQL().QueryRowContext(ctx, `SELECT m.node_id FROM paperboat.managed_node_authority m JOIN paperboat.operator_regions r ON r.id=m.region_id JOIN paperboat.control_tunnel_nodes n ON n.id=m.node_id WHERE m.runtime_hash=$1 AND m.retired_at IS NULL AND r.enabled AND n.state<>'retired'`, h[:]).Scan(&node)
	if err == sql.ErrNoRows {
		return "", nodeauth.ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return node, nil
}
func (s *Service) RuntimeCredential(ctx context.Context, node string) (string, error) {
	var cipher []byte
	var enabled bool
	err := s.store.SQL().QueryRowContext(ctx, `SELECT m.runtime_ciphertext,(m.retired_at IS NULL AND r.enabled AND n.state<>'retired') FROM paperboat.managed_node_authority m JOIN paperboat.operator_regions r ON r.id=m.region_id JOIN paperboat.control_tunnel_nodes n ON n.id=m.node_id WHERE m.node_id=$1`, node).Scan(&cipher, &enabled)
	if err == sql.ErrNoRows {
		return "", nodeauth.ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if !enabled {
		return "", ErrForbidden
	}
	return secrets.Decrypt(s.key, cipher)
}

func (s *Service) IsSelfHosted(context.Context, string) (bool, error) { return false, nil }
func readNodeTx(ctx context.Context, tx *db.Tx, id string, out *Node) error {
	return tx.QueryRow(ctx, `SELECT n.id,m.region_id,n.edge_pool,n.failure_domain,n.state,n.endpoint_host,n.endpoint_tcp_port,n.endpoint_quic_port,n.roles,n.transports,n.capacity_limit,m.revision,n.node_generation,n.last_heartbeat_at,n.registry_expires_at,(n.ready AND n.registry_expires_at>now() AND n.last_heartbeat_at>$2 AND r.enabled AND m.retired_at IS NULL) FROM control_tunnel_nodes n JOIN managed_node_authority m ON m.node_id=n.id JOIN operator_regions r ON r.id=m.region_id WHERE n.id=$1`, id, time.Now().Add(-controlplane.ControlTunnelNodeStaleAfter())).Scan(&out.ID, &out.RegionID, &out.EdgePool, &out.FailureDomain, &out.State, &out.EndpointHost, &out.EndpointTCPPort, &out.EndpointQUICPort, &out.Roles, &out.Transports, &out.CapacityLimit, &out.Revision, &out.NodeGeneration, &out.ObservedAt, &out.RegistryExpiresAt, &out.Ready)
}

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)
var regionIdentifier = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)
var endpointName = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)

func validCommon(actor, key, target, reason string) bool {
	return identifier.MatchString(actor) && len(key) >= 8 && len(key) <= 200 && identifier.MatchString(key) && identifier.MatchString(target) && identifier.MatchString(reason)
}

type nodeConfig struct {
	RegionID               string   `json:"region_id"`
	EdgePool               string   `json:"edge_pool"`
	FailureDomain          string   `json:"failure_domain"`
	EndpointHost           string   `json:"endpoint_host"`
	EndpointTCPPort        int32    `json:"endpoint_tcp_port"`
	EndpointQUICPort       int32    `json:"endpoint_quic_port"`
	Roles                  []string `json:"roles"`
	Transports             []string `json:"transports"`
	CapacityLimit          int64    `json:"capacity_limit"`
	UsagePublicKey         []byte   `json:"usage_public_key,omitempty"`
	PeerWireGuardPublicKey []byte   `json:"peer_wireguard_public_key,omitempty"`
	PeerDiscoPublicKey     []byte   `json:"peer_disco_public_key,omitempty"`
	PeerVirtualAddress     string   `json:"peer_virtual_address,omitempty"`
}

func publicConfig(in NodeMutation) nodeConfig {
	return nodeConfig{in.RegionID, in.EdgePool, in.FailureDomain, in.EndpointHost, in.EndpointTCPPort, in.EndpointQUICPort, in.Roles, in.Transports, in.CapacityLimit, in.UsagePublicKey, in.PeerWireGuardPublicKey, in.PeerDiscoPublicKey, in.PeerVirtualAddress}
}
func validNode(in NodeMutation) bool {
	if !regionIdentifier.MatchString(in.RegionID) || !identifier.MatchString(in.EdgePool) || !identifier.MatchString(in.FailureDomain) || !endpointName.MatchString(in.EndpointHost) || in.EndpointTCPPort <= 0 || in.EndpointTCPPort > 65535 || in.EndpointQUICPort <= 0 || in.EndpointQUICPort > 65535 || in.EndpointTCPPort == in.EndpointQUICPort || in.CapacityLimit <= 0 || len(in.Roles) < 1 || len(in.Roles) > 2 || len(in.Transports) < 1 || len(in.Transports) > 4 || !members(in.Roles, []string{"edge", "relay", "peer_relay"}) || !members(in.Transports, []string{"http3", "http2", "derp_quic", "derp_wss", "peer_relay_udp"}) {
		return false
	}
	if contains(in.Roles, "edge") && (len(in.UsagePublicKey) != 32 || (!contains(in.Transports, "http3") && !contains(in.Transports, "http2"))) {
		return false
	}
	if contains(in.Roles, "relay") && (in.CapacityLimit > relaylimits.MaxConnectionsPerNode || !contains(in.Transports, "derp_quic") || !contains(in.Transports, "derp_wss")) {
		return false
	}
	if contains(in.Roles, "peer_relay") {
		address, err := netip.ParseAddr(in.PeerVirtualAddress)
		if err != nil || !address.Is6() || !netip.MustParsePrefix("fd7a:115c:a1e0::/48").Contains(address) || !contains(in.Roles, "relay") || !contains(in.Transports, "peer_relay_udp") || len(in.PeerWireGuardPublicKey) != 32 || len(in.PeerDiscoPublicKey) != 32 {
			return false
		}
	}
	if !contains(in.Roles, "peer_relay") && (len(in.PeerWireGuardPublicKey) != 0 || len(in.PeerDiscoPublicKey) != 0 || in.PeerVirtualAddress != "") {
		return false
	}
	if len(in.UsagePublicKey) != 0 && len(in.UsagePublicKey) != 32 {
		return false
	}
	return true
}
func members(values, allowed []string) bool {
	seen := map[string]bool{}
	for _, v := range values {
		if seen[v] {
			return false
		}
		seen[v] = true
		if !contains(allowed, v) {
			return false
		}
	}
	return true
}
func contains(v []string, w string) bool {
	for _, x := range v {
		if x == w {
			return true
		}
	}
	return false
}
func nullableBytes(v []byte) any {
	if len(v) == 0 {
		return nil
	}
	return v
}
func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
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
