package sync

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/pinksaucepasta/paperboat/server/internal/machineaddress"
	"reflect"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"sort"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/machineservices"
	pb "github.com/pinksaucepasta/paperboat/server/internal/sync/proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type SQLAuthority struct {
	DB       *db.DB
	Services *machineservices.Service
}

func (a *SQLAuthority) Snapshot(ctx context.Context, p auth.ClientPrincipal, source string) (*pb.SyncResponse, error) {
	out := &pb.SyncResponse{Peers: []*pb.PeerUpdate{}, PostureCheck: &pb.MachinePostureCheck{Allowed: true, Approved: true}}
	if source != "" {
		var owned bool
		err := a.DB.Pool().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.user_machines WHERE id=$1 AND user_id=$2 AND revoked_at IS NULL AND deleted_at IS NULL)`, source, p.User.ID).Scan(&owned)
		if err != nil {
			return nil, err
		}
		if !owned {
			return nil, status.Error(codes.PermissionDenied, "source machine is unavailable")
		}
		ip, _, err := a.lease(ctx, p.User.ID, source)
		if err != nil {
			return nil, err
		}
		out.AssignedIp = ip
	}
	items, err := a.Services.List(ctx, p.User.ID)
	if err != nil {
		return nil, err
	}

	// Reuse canonical machine visibility, including machines without TCP services.
	rows, err := a.DB.Pool().Query(ctx, `SELECT m.id,m.alias,m.online,m.installation_generation,
 paperboat.machine_capability_allowed($1,m.id,'terminal'),paperboat.machine_capability_allowed($1,m.id,'managed_ssh')
 FROM paperboat.user_machines m WHERE m.deleted_at IS NULL AND m.revoked_at IS NULL AND
 (paperboat.machine_management_allowed($1,m.id) OR EXISTS(SELECT 1 FROM unnest(ARRAY['terminal','exec','managed_ssh','files','preview_manage','tunnel_manage']) cap WHERE paperboat.machine_capability_allowed($1,m.id,cap))) ORDER BY m.id LIMIT 129`, p.User.ID)
	if err != nil {
		return nil, err
	}
	type candidate struct {
		peer       *pb.PeerUpdate
		generation int64
	}
	candidates := map[string]candidate{}
	for rows.Next() {
		peer := &pb.PeerUpdate{Capabilities: &pb.MachineCapabilities{}}
		var generation int64
		if err = rows.Scan(&peer.PeerId, &peer.Alias, &peer.Online, &generation, &peer.Capabilities.CanHostTerminals, &peer.Capabilities.CanMountSsh); err != nil {
			rows.Close()
			if closeErr := rows.Err(); closeErr != nil {
				return nil, errors.Join(err, closeErr)
			}
			return nil, err
		}
		candidates[peer.PeerId] = candidate{peer, generation}
	}
	rows.Close()
	err = rows.Err()
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		c, ok := candidates[item.MachineID]
		if !ok {
			c = candidate{&pb.PeerUpdate{PeerId: item.MachineID, Alias: item.Alias, Online: true, Capabilities: &pb.MachineCapabilities{}}, item.InstallationGeneration}
		}
		for _, port := range item.Services {
			c.peer.ExportedPorts = append(c.peer.ExportedPorts, int32(port.Port))
		}
		c.peer.Capabilities.CanExportPorts = len(c.peer.ExportedPorts) > 0
		c.peer.Capabilities.ExportedPorts = c.peer.ExportedPorts
		candidates[item.MachineID] = c
	}
	if len(candidates) > 128 {
		return nil, status.Error(codes.ResourceExhausted, "authorized machine inventory exceeds sync limit")
	}
	ids := make([]string, 0, len(candidates))
	for id := range candidates {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		c := candidates[id]
		err = a.DB.Pool().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM paperboat.peer_network_identities n JOIN paperboat.peer_endpoint_certificates c ON c.fingerprint=n.quic_certificate_fingerprint WHERE n.machine_id=$1 AND n.machine_generation=$2 AND n.revoked_at IS NULL AND c.revoked_at IS NULL AND c.expires_at>now())`, id, c.generation).Scan(&c.peer.Approved)
		if err != nil {
			return nil, err
		}
		// Unenrolled/revoked identities remain visible as pending, without usable network state.
		if c.peer.Approved {
			ip, tags, err := a.lease(ctx, p.User.ID, id)
			if err != nil {
				return nil, err
			}
			c.peer.AssignedIp = ip
			c.peer.Tags = tags
		} else {
			c.peer.Capabilities = &pb.MachineCapabilities{}
			c.peer.ExportedPorts = nil
		}
		out.Peers = append(out.Peers, c.peer)
	}

	return out, nil
}

// lease is the machine's local address as the viewer account sees it (its own lease, else the owner's until
// per-viewer leases exist; PLAN 3b "Multi-account hosts"), allocating the owner's lease if the machine has none.
func (a *SQLAuthority) lease(ctx context.Context, viewer, machine string) (string, []string, error) {
	var slot int
	var encoded []byte
	if _, err := a.DB.Pool().Exec(ctx, `SELECT paperboat.ensure_control_sync_lease($1)`, machine); err != nil {
		if postgresErrorCode(err, "54000") {
			return "", nil, status.Error(codes.ResourceExhausted, "machine address pool exhausted; remove retired machines before retrying")
		}
		return "", nil, err
	}
	err := a.DB.Pool().QueryRow(ctx, `SELECT address_slot,tags FROM paperboat.control_sync_leases WHERE machine_id=$1
 ORDER BY account_id=$2 DESC LIMIT 1`, machine, viewer).Scan(&slot, &encoded)
	if err != nil {
		return "", nil, err
	}
	var tags []string
	if err = json.Unmarshal(encoded, &tags); err != nil {
		return "", nil, err
	}
	return machineaddress.FromSlot(slot).String(), tags, nil
}
func (a *SQLAuthority) SetTags(ctx context.Context, p auth.ClientPrincipal, machine string, tags []string) (resultErr error) {
	tx, err := a.DB.Pool().Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), RequestTimeout)
		cleanupErr := tx.Rollback(cleanupCtx)
		cancel()
		if cleanupErr != nil && cleanupErr != pgx.ErrTxClosed {
			resultErr = &transactionCleanupError{operation: resultErr, cleanup: cleanupErr}
		}
	}()
	var allowed bool
	err = tx.QueryRow(ctx, `SELECT paperboat.machine_management_allowed($1,id) FROM paperboat.user_machines WHERE id=$2 AND revoked_at IS NULL AND deleted_at IS NULL FOR UPDATE`, p.User.ID, machine).Scan(&allowed)
	if postgresNoRows(err) || err == nil && !allowed {
		return status.Error(codes.PermissionDenied, "machine is unavailable")
	}
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `SELECT paperboat.ensure_control_sync_lease($1)`, machine); err != nil {
		return err
	}
	if tags == nil {
		tags = []string{}
	}
	encoded, err := json.Marshal(tags)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE paperboat.control_sync_leases SET tags=$2 WHERE machine_id=$1`, machine, encoded); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type transactionCleanupError struct {
	operation error
	cleanup   error
}

func (*transactionCleanupError) Error() string { return "sync transaction and cleanup failed" }
func (e *transactionCleanupError) Unwrap() []error {
	causes := make([]error, 0, 2)
	if e.operation != nil {
		causes = append(causes, e.operation)
	}
	if e.cleanup != nil {
		causes = append(causes, e.cleanup)
	}
	return causes
}

func postgresNoRows(err error) bool {
	return postgresSoleCause(err) == pgx.ErrNoRows
}

func postgresErrorCode(err error, want string) bool {
	cause := postgresSoleCause(err)
	if cause == nil || nilAuthorityError(cause) {
		return false
	}
	postgres, ok := cause.(*pgconn.PgError)
	return ok && postgres.Code == want
}

func postgresSoleCause(err error) error {
	for visited := 0; err != nil && visited < 32; visited++ {
		if nilAuthorityError(err) {
			return nil
		}
		if joined, ok := err.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) != 1 {
				return nil
			}
			err = children[0]
			continue
		}
		wrapped, ok := err.(interface{ Unwrap() error })
		if !ok {
			return err
		}
		err = wrapped.Unwrap()
	}
	return nil
}

func nilAuthorityError(err error) bool {
	value := reflect.ValueOf(err)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}
