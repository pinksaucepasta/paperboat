package resourceusage

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/shared/peerconsumer"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
)

var ErrDenied = errors.New("usage reporting authority unavailable")

const MaxReports = 64
const DeliveryWindow = 7 * 24 * time.Hour
const MaxCounter int64 = 1 << 40 // 1 TiB per stream/path/minute; bounds corrupt reports.
type NativeReport struct {
	AccessSessionID string    `json:"access_session_id"`
	StreamID        string    `json:"stream_id"`
	Consumer        string    `json:"consumer"`
	Mode            string    `json:"mode"`
	NodeID          string    `json:"node_id"`
	IntervalStart   time.Time `json:"interval_start"`
	IntervalEnd     time.Time `json:"interval_end"`
	Upload          int64     `json:"upload_bytes"`
	Download        int64     `json:"download_bytes"`
	// PeerAccountID marks a stream the host admitted from its own signed rules (every managed SSH stream, D1;
	// exec or terminal offline): it has no access session, AccessSessionID is the host machine itself.
	PeerAccountID string `json:"peer_account_id,omitempty"`
}
type NativeBatch struct {
	Reports             []NativeReport `json:"reports"`
	UnrecordedBytes     int64          `json:"unrecorded_bytes,omitempty"`
	ExpiredBytes        int64          `json:"expired_bytes,omitempty"`
	RecordingIncomplete bool           `json:"recording_incomplete,omitempty"`
}

func (r NativeReport) validate(now time.Time) error {
	if r.AccessSessionID == "" || len(r.AccessSessionID) > 256 || r.StreamID == "" || len(r.StreamID) > 128 || len(r.NodeID) > 256 || strings.TrimSpace(r.StreamID) != r.StreamID || r.Upload < 0 || r.Download < 0 || r.Upload > MaxCounter || r.Download > MaxCounter {
		return ErrInvalid
	}
	if r.IntervalStart.IsZero() || !r.IntervalStart.Equal(r.IntervalStart.UTC().Truncate(time.Minute)) || r.IntervalEnd.Before(r.IntervalStart) || r.IntervalEnd.After(r.IntervalStart.Add(time.Minute)) || r.IntervalEnd.After(now.Add(time.Minute)) || now.Sub(r.IntervalEnd) > DeliveryWindow {
		return ErrInvalid
	}
	switch r.Mode {
	case "direct":
		if r.NodeID != "" {
			return ErrInvalid
		}
	case "relay":
	case "edge":
	case "unknown":
		if r.NodeID != "" {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	// Every stream with network access is metered by its host (shared/peerconsumer).
	if peerconsumer.NetworkCapability(r.Consumer) == "" {
		return ErrInvalid
	}
	// Host rules admit only the offline consumers (peerconsumer.Offline), never through an edge.
	if r.PeerAccountID != "" && (len(r.PeerAccountID) > 256 || r.Mode == "edge" || !peerconsumer.Offline(r.Consumer)) {
		return ErrInvalid
	}
	return nil
}

// consumerCapability is the access-policy capability a consumer's usage is attributed under: its network scope
// capability, except that file transfers are the policy's "files".
func consumerCapability(consumer string) string {
	if consumer == peerconsumer.FileTransfer {
		return "files"
	}
	return peerconsumer.NetworkCapability(consumer)
}

type NativeReceipt struct {
	Accepted        int   `json:"accepted"`
	RejectedIndices []int `json:"rejected_indices"`
}

func (s *Service) RecordNative(ctx context.Context, machine string, generation int64, batch NativeBatch) (NativeReceipt, error) {
	receipt := NativeReceipt{RejectedIndices: []int{}}
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now().UTC()
	}
	if machine == "" || generation < 1 || len(batch.Reports) > MaxReports || batch.UnrecordedBytes < 0 || batch.ExpiredBytes < 0 {
		return receipt, ErrInvalid
	}
	for _, r := range batch.Reports {
		if err := r.validate(now); err != nil {
			return receipt, err
		}
	}
	// READ COMMITTED, like control usage reports: the workspace's period row is debited under its row lock, so
	// concurrent machines queue instead of aborting each other with 40001. This machine's batches are serialized by
	// an advisory lock, so the previous-value and capacity reads below cannot be raced by a replay of the same report.
	err := s.Store.InReadCommittedTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('native-usage:'||$1,0))`, machine); err != nil {
			return fmt.Errorf("lock native usage: %w", err)
		}
		receipt = NativeReceipt{RejectedIndices: []int{}}
		for index, r := range batch.Reports {
			var ws, kind, id, name, machineName, admittedNode, resourceRoute string
			var allowed bool
			var actor sql.NullString
			var err error
			if r.PeerAccountID == "" {
				err = tx.QueryRow(ctx, `SELECT workspace_id,resource_kind,resource_id,resource_name,machine_name,$4=ANY(capabilities),edge_node_id,resource_route_id,actor_account_id FROM native_usage_authorities WHERE access_session_id=$1 AND machine_id=$2 AND installation_generation=$3 AND retain_until>$5 AND admitted_at<=$6 AND expires_at>=$6 AND ($7<>'edge' OR (edge_node_id<>'' AND ($8='' OR edge_node_id=$8))) FOR SHARE`, r.AccessSessionID, machine, generation, consumerCapability(r.Consumer), now, r.IntervalEnd, r.Mode, r.NodeID).Scan(&ws, &kind, &id, &name, &machineName, &allowed, &admittedNode, &resourceRoute, &actor)
			} else {
				// A host-rules stream is attributed like its audit event: by the access decision online descriptors use
				// for the peer the host's native session proved, in the workspace that decides it.
				// shortcut: decided at report time, so bytes reported after the peer lost access are dropped (not charged).
				err = tx.QueryRow(ctx, `SELECT d.workspace,'machine',m.id,m.alias,m.alias,d.reason IS NULL AND $1=m.id,'','',$4::text FROM user_machines m CROSS JOIN paperboat.machine_access_decision($4,m.id,$5,false,NULL) d WHERE m.id=$2 AND m.installation_generation=$3 AND m.deleted_at IS NULL`, r.AccessSessionID, machine, generation, r.PeerAccountID, consumerCapability(r.Consumer)).Scan(&ws, &kind, &id, &name, &machineName, &allowed, &admittedNode, &resourceRoute, &actor)
			}
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if err != nil || !allowed {
				receipt.RejectedIndices = append(receipt.RejectedIndices, index)
				continue
			}
			if kind == "machine" {
				kind = r.Consumer
				name = machineName + " · " + strings.ReplaceAll(kind, "_", " ")
			}
			if r.Mode == "edge" {
				r.NodeID = admittedNode
			}
			hosting, nodeName := "none", ""
			if r.Mode == "unknown" {
				hosting = "unknown"
			}
			if r.Mode == "relay" && r.NodeID == "" {
				hosting = "unknown"
			}
			if (r.Mode == "relay" || r.Mode == "edge") && r.NodeID != "" {
				// A machine can observe bytes but cannot disclose an arbitrary
				// private operator's identity by naming its node. Scope visibility
				// deliberately survives node retirement and session revocation;
				// current placement/readiness is not historical reporting authority.
				err := tx.QueryRow(ctx, `SELECT CASE WHEN i.scope_kind IN ('account','team') THEN 'self_hosted' ELSE 'managed' END,coalesce(i.name,n.id)
 FROM control_tunnel_nodes n LEFT JOIN selfhost_installations i ON i.node_id=n.id
 WHERE n.id=$1 AND (i.id IS NULL OR $4='edge' OR paperboat.selfhost_scope_visible(i.scope_kind,i.scope_id,(SELECT user_id FROM user_machines WHERE id=$2 AND installation_generation=$3)))`, r.NodeID, machine, generation, r.Mode).Scan(&hosting, &nodeName)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return err
				}
				if errors.Is(err, sql.ErrNoRows) {
					hosting = "unknown"
					nodeName = r.NodeID
				}
			}
			// Stable per-minute cumulative records permit replay and out-of-order
			// updates without creating additional billable measurements.
			key, _ := json.Marshal([]any{machine, generation, r.AccessSessionID, r.StreamID, r.Consumer, r.Mode, r.NodeID, r.IntervalStart.UTC(), r.PeerAccountID})
			digest := sha256.Sum256(key)
			operation := "native:" + hex.EncodeToString(digest[:])
			var capacity bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM resource_bandwidth_events WHERE operation_id=$1) OR (SELECT count(*) FROM resource_bandwidth_events WHERE machine_id=$2 AND interval_start=$3)<8192`, operation, machine, r.IntervalStart).Scan(&capacity); err != nil {
				return err
			}
			if !capacity {
				receipt.RejectedIndices = append(receipt.RejectedIndices, index)
				continue
			}
			// Reports are cumulative per stream and minute and may be replayed or arrive
			// out of order, so only growth of the stored record is charged, and only
			// relay bytes carried by a hosted (managed) node: direct traffic is free and
			// customer-run relays never consume Paperboat's allowance.
			var previousUp, previousDown int64
			if err := tx.QueryRow(ctx, `SELECT upload_bytes,download_bytes FROM resource_bandwidth_events WHERE operation_id=$1`, operation).Scan(&previousUp, &previousDown); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			grown := max(previousUp, r.Upload) + max(previousDown, r.Download) - previousUp - previousDown
			if s.Debiter != nil && r.Mode == "relay" && hosting == "managed" && grown > 0 {
				if _, _, err := s.Debiter.DebitWorkspaceBandwidthTx(ctx, tx, ws, grown, r.IntervalEnd); err != nil {
					return fmt.Errorf("debit native relay bandwidth: %w", err)
				}
			}
			_, err = tx.Exec(ctx, `INSERT INTO resource_bandwidth_events(operation_id,workspace_id,resource_kind,resource_id,resource_name,machine_id,machine_name,mode,hosting,node_id,node_name,upload_bytes,download_bytes,interval_start,interval_end,resource_route_id,actor_account_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17) ON CONFLICT(operation_id) DO UPDATE SET actor_account_id=coalesce(resource_bandwidth_events.actor_account_id,EXCLUDED.actor_account_id),upload_bytes=greatest(resource_bandwidth_events.upload_bytes,EXCLUDED.upload_bytes),download_bytes=greatest(resource_bandwidth_events.download_bytes,EXCLUDED.download_bytes),interval_end=greatest(resource_bandwidth_events.interval_end,EXCLUDED.interval_end)`, operation, ws, kind, id, name, machine, machineName, r.Mode, hosting, r.NodeID, nodeName, r.Upload, r.Download, r.IntervalStart, r.IntervalEnd, resourceRoute, actor)
			if err != nil {
				return fmt.Errorf("persist native usage: %w", err)
			}
			receipt.Accepted++
		}
		if err := recordNativeReportingStatus(ctx, tx, machine, generation, batch, len(receipt.RejectedIndices), now); err != nil {
			return err
		}
		// Bounded opportunistic retention avoids a new maintenance service and
		// never erases retry keys inside the accepted delivery window.
		if _, err := tx.Exec(ctx, `DELETE FROM native_usage_authorities WHERE access_session_id IN (SELECT access_session_id FROM native_usage_authorities WHERE retain_until<$1 LIMIT 500)`, now); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `DELETE FROM resource_bandwidth_events WHERE operation_id IN (SELECT operation_id FROM resource_bandwidth_events WHERE interval_end<$1 LIMIT 500)`, now.Add(-MaxWindow))
		return err
	})
	return receipt, err
}

// RecordBrowserAttached keeps a browser terminal attachment counted until until (it is still attached);
// RecordBrowserDetached ends it now. Neither changes the authority's expires_at.
func (s *Service) RecordBrowserAttached(ctx context.Context, attachment string, until time.Time) error {
	_, err := s.Store.SQL().ExecContext(ctx, `UPDATE native_usage_authorities SET attached_until=$2 WHERE access_session_id=$1 AND resource_kind='terminal' AND attached_until>now()`, attachment, until)
	return err
}

// RecordBrowserDetached ends the attachment unless a resume extended attached_until past until (the
// value the detaching connection last wrote): then the resumed connection owns it and it stays attached.
func (s *Service) RecordBrowserDetached(ctx context.Context, attachment string, until time.Time) error {
	_, err := s.Store.SQL().ExecContext(ctx, `UPDATE native_usage_authorities SET attached_until=least(attached_until,now()) WHERE access_session_id=$1 AND resource_kind='terminal' AND attached_until<=$2`, attachment, until)
	return err
}

// ResumeBrowserAttached re-attaches a terminal attachment whose edge control connection was cut,
// until until. Only that terminal's attachment through node, still attached at since, qualifies.
func (s *Service) ResumeBrowserAttached(ctx context.Context, attachment, terminal, node string, since, until time.Time) error {
	result, err := s.Store.SQL().ExecContext(ctx, `UPDATE native_usage_authorities SET attached_until=$5 WHERE access_session_id=$1 AND resource_kind='terminal' AND resource_id=$2 AND edge_node_id=$3 AND attached_until>$4`, attachment, terminal, node, since, until)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return cmp.Or(err, ErrDenied)
	}
	return nil
}

// RecordBrowserAdmission snapshots the exact issued browser attachment. A
// revoked/deleted grant can still report earlier bytes but cannot grant access.
func (s *Service) RecordBrowserAdmission(ctx context.Context, attachment, machine, terminal, node, actor string, expires time.Time) error {
	result, err := s.Store.SQL().ExecContext(ctx, `INSERT INTO native_usage_authorities(access_session_id,machine_id,installation_generation,workspace_id,resource_kind,resource_id,resource_name,machine_name,capabilities,admitted_at,expires_at,retain_until,edge_node_id,actor_account_id,attached_until) SELECT $1,m.id,m.installation_generation,coalesce('team:'||t.owner_team_id,'personal:'||t.owner_account),'terminal',t.id,coalesce(t.title,t.name),m.alias,ARRAY['terminal'],now(),$4::timestamptz,$4::timestamptz+interval '7 days',$5,coalesce(nullif($6,''),t.owner_account),$4::timestamptz FROM user_machine_terminal_sessions t JOIN user_machines m ON m.id=t.user_machine_id WHERE t.id=$3 AND m.id=$2 ON CONFLICT(access_session_id) DO NOTHING`, attachment, machine, terminal, expires, node, actor)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrDenied
	}
	return nil
}
