package tunnelv1

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
)

func updateTunnelFields(ctx context.Context, tx *db.Tx, input PatchRecord, now time.Time) (dbsqlc.Tunnel, error) {
	var name sql.NullString
	if input.Name != nil {
		name = nullableString(*input.Name)
	}
	var accessMode sql.NullString
	if input.AccessMode != nil {
		accessMode = nullableString(*input.AccessMode)
	}
	var expiresAt sql.NullTime
	if input.ExpiresAt != nil {
		expiresAt = sql.NullTime{Time: input.ExpiresAt.UTC(), Valid: true}
	}
	tunnel, err := tx.Queries().UpdatePreviewTunnelFieldsV1(ctx, dbsqlc.UpdatePreviewTunnelFieldsV1Params{
		NameSet: input.Name != nil, Name: name, AccessModeSet: input.AccessMode != nil, AccessMode: accessMode,
		ExpiresAtSet: input.ExpirySet, ExpiresAt: expiresAt, Now: now, ID: input.TunnelID,
		AccountID: input.AccountID, ExpectedGeneration: input.ExpectedGeneration,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return dbsqlc.Tunnel{}, ErrGenerationConflict
	}
	return tunnel, translate(err)
}

func patchChanges(current dbsqlc.Tunnel, input PatchRecord) bool {
	if input.Name != nil && current.Name != *input.Name {
		return true
	}
	if input.AccessMode != nil && current.AccessMode != *input.AccessMode {
		return true
	}
	if input.ExpirySet {
		if input.ExpiresAt == nil {
			return current.ExpiresAt.Valid
		}
		return !current.ExpiresAt.Valid || !current.ExpiresAt.Time.Equal(input.ExpiresAt.UTC())
	}
	return false
}

func expiryOutcome(previous, updated dbsqlc.Tunnel) string {
	if previous.SummaryCode == updated.SummaryCode || updated.SummaryCode != "expired" {
		return "unchanged"
	}
	return "changed"
}

func stateOperationName(state string) string {
	switch state {
	case DesiredPaused:
		return "paused"
	case DesiredActive:
		return "resumed"
	case DesiredDeleted:
		return "deleted"
	default:
		return "state_changed"
	}
}

func validateCreateRecord(input CreateRecord) error {
	for name, value := range map[string]string{
		"operation_id": input.OperationID, "tunnel_id": input.TunnelID,
		"stable_endpoint_id": input.StableEndpointID, "stable_endpoint": input.StableEndpoint,
		"account_id": input.AccountID, "name": input.Name, "access_mode": input.AccessMode,
		"idempotency_key": input.IdempotencyKey, "actor_id": input.ActorID,
		"host_id": input.HostID, "actor_type": input.ActorType, "correlation_id": input.CorrelationID,
		"audit_event_id": input.AuditEventID,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%w: %s is required", ErrInvalidInput, name)
		}
	}
	if input.RequestHash == ([sha256.Size]byte{}) {
		return fmt.Errorf("%w: request hash is required", ErrInvalidInput)
	}
	if _, err := validateTunnelName(input.Name); err != nil {
		return err
	}
	if err := validateStableEndpointForID(input.StableEndpoint, input.StableEndpointID); err != nil {
		return err
	}
	if !validActorType(input.ActorType) {
		return fmt.Errorf("%w: actor type is invalid", ErrInvalidInput)
	}
	if input.AccessMode != AccessPublic && input.AccessMode != AccessPrivate {
		return fmt.Errorf("%w: access mode is invalid", ErrInvalidInput)
	}
	return validateOrigin(input.Origin)
}

func validatePatchRecord(input PatchRecord) error {
	for name, value := range map[string]string{
		"operation_id": input.OperationID, "audit_event_id": input.AuditEventID,
		"tunnel_id": input.TunnelID, "account_id": input.AccountID,
		"idempotency_key": input.IdempotencyKey, "actor_id": input.ActorID,
		"actor_type": input.ActorType, "correlation_id": input.CorrelationID,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%w: %s is required", ErrInvalidInput, name)
		}
	}
	if input.ExpectedGeneration < 1 || input.RequestHash == ([sha256.Size]byte{}) {
		return fmt.Errorf("%w: generation and request hash are required", ErrInvalidInput)
	}
	if !validActorType(input.ActorType) {
		return fmt.Errorf("%w: actor type is invalid", ErrInvalidInput)
	}
	if input.Name == nil && input.AccessMode == nil && !input.ExpirySet {
		return fmt.Errorf("%w: patch must contain a mutable field", ErrInvalidInput)
	}
	if input.Name != nil && strings.TrimSpace(*input.Name) == "" {
		return fmt.Errorf("%w: name cannot be empty", ErrInvalidInput)
	}
	if input.Name != nil {
		if _, err := validateTunnelName(*input.Name); err != nil {
			return err
		}
	}
	if input.AccessMode != nil && *input.AccessMode != AccessPublic && *input.AccessMode != AccessPrivate {
		return fmt.Errorf("%w: access mode is invalid", ErrInvalidInput)
	}
	if input.ExpiresAt != nil && !input.Now.IsZero() && !input.ExpiresAt.After(input.Now.UTC()) {
		return fmt.Errorf("%w: expiry must be in the future", ErrInvalidInput)
	}
	return nil
}

func validateStateRecord(input StateRecord) error {
	for name, value := range map[string]string{
		"operation_id": input.OperationID, "audit_event_id": input.AuditEventID,
		"tunnel_id": input.TunnelID, "account_id": input.AccountID,
		"idempotency_key": input.IdempotencyKey, "actor_id": input.ActorID,
		"actor_type": input.ActorType, "correlation_id": input.CorrelationID,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%w: %s is required", ErrInvalidInput, name)
		}
	}
	if input.ExpectedGeneration < 1 || input.RequestHash == ([sha256.Size]byte{}) {
		return fmt.Errorf("%w: generation and request hash are required", ErrInvalidInput)
	}
	if !validActorType(input.ActorType) {
		return fmt.Errorf("%w: actor type is invalid", ErrInvalidInput)
	}
	switch input.DesiredState {
	case DesiredActive, DesiredPaused, DesiredDeleted:
		return nil
	default:
		return fmt.Errorf("%w: unsupported desired state", ErrInvalidInput)
	}
}

func validateOrigin(origin OriginRequest) error {
	return validateOriginRequest(origin)
}

func nullableString(value string) sql.NullString {
	return sql.NullString{String: value, Valid: strings.TrimSpace(value) != ""}
}

func actorUserID(actorType, actorID string) sql.NullString {
	if actorType != "user" {
		return sql.NullString{}
	}
	return nullableString(actorID)
}

// auditActorID permits a host mutation to retain the authenticated user as
// created_by_actor_id while attributing its audit event to the verified host.
// Records constructed by older callers fall back to ActorID.
func auditActorID(actorID, explicit string) string {
	if strings.TrimSpace(explicit) != "" {
		return explicit
	}
	return actorID
}

func validActorType(actorType string) bool {
	switch actorType {
	case "user", "host", "system", "edge":
		return true
	default:
		return false
	}
}

func translate(err error) error {
	if err == nil {
		return nil
	}
	pgxNoRows, sole, leaf := solePostgresError(err)
	if !sole {
		return err
	}
	if pgxNoRows {
		return ErrNotFound
	}
	pgErr, ok := leaf.(*pgconn.PgError)
	if ok && pgErr != nil {
		switch pgErr.Code {
		case "23505":
			if pgErr.ConstraintName == "tunnels_stable_endpoint_key" || pgErr.ConstraintName == "managed_hostname_reserved" {
				return ErrEndpointConflict
			}
			if pgErr.ConstraintName == "shared_443_hostname_mode_conflict" {
				return fmt.Errorf("%w: %w", ErrRouteConflict, ErrHostnameModeConflict)
			}
			if strings.Contains(pgErr.ConstraintName, "tunnels_account_id_name") {
				return fmt.Errorf("%w: %s", ErrNameConflict, pgErr.ConstraintName)
			}
			if strings.Contains(pgErr.ConstraintName, "tunnel_routes_tunnel_id_name") || strings.Contains(pgErr.ConstraintName, "tunnel_routes_id_tunnel") {
				return fmt.Errorf("%w: %s", ErrRouteConflict, pgErr.ConstraintName)
			}
			if strings.Contains(pgErr.ConstraintName, "tunnel_domains_hostname") {
				return fmt.Errorf("%w: %s", ErrDomainConflict, pgErr.ConstraintName)
			}
			return fmt.Errorf("%w: %s", ErrConflict, pgErr.ConstraintName)
		case "23503", "23514", "23502":
			return fmt.Errorf("%w: %s", ErrInvalidInput, pgErr.ConstraintName)
		}
	}
	return err
}

// solePostgresError follows only bounded linear wrappers and one-child joins.
// Transaction cleanup joins are intentionally left intact so a constraint or
// no-row cause cannot hide an independent rollback failure.
func solePostgresError(err error) (pgxNoRows, ok bool, leaf error) {
	defer func() {
		if recover() != nil {
			pgxNoRows, ok, leaf = false, false, nil
		}
	}()
	pgxNoRows = false
	for depth := 0; err != nil && depth < 32; depth++ {
		if err == pgx.ErrNoRows {
			pgxNoRows = true
		}
		if joined, isJoin := err.(interface{ Unwrap() []error }); isJoin {
			children := joined.Unwrap()
			if len(children) != 1 {
				return false, false, nil
			}
			err = children[0]
			continue
		}
		if wrapped, isWrapped := err.(interface{ Unwrap() error }); isWrapped {
			err = wrapped.Unwrap()
			continue
		}
		return pgxNoRows, true, err
	}
	return false, false, nil
}

func isSolePostgresNoRows(err error) bool {
	noRows, sole, _ := solePostgresError(err)
	return sole && noRows
}

func newRouteID(tunnelID string) string {
	// Route IDs are opaque to callers, but retaining the complete durable tunnel
	// identity makes the deterministic child key collision-free for generated
	// tunnel IDs and searchable in incident logs.
	return "rte_" + tunnelID + "_default"
}

// decoratePublicEndpoints changes only API return values. The durable managed
// endpoint remains the internal identity used by config generation and switching.
func (r *SQLRepository) decoratePublicEndpoints(ctx context.Context, tunnels []dbsqlc.Tunnel) error {
	if len(tunnels) == 0 {
		return nil
	}
	ids := make([]string, len(tunnels))
	index := make(map[string]int, len(tunnels))
	for i, t := range tunnels {
		ids[i] = t.ID
		index[t.ID] = i
	}
	rows, err := r.db.Pool().Query(ctx, `SELECT t.id,coalesce((SELECT 'https://'||d.hostname FROM paperboat.tunnel_domains d JOIN paperboat.tunnel_routes route ON route.id=d.route_id WHERE d.tunnel_id=t.id AND d.account_id=t.account_id AND d.deleted_at IS NULL AND d.ownership_state='verified' AND d.conflict_state='clear' AND d.match_type='exact' AND route.protocol='http' AND route.desired_state='active' AND route.deleted_at IS NULL ORDER BY d.hostname LIMIT 1),''),EXISTS(SELECT 1 FROM paperboat.tunnel_routes needed WHERE needed.tunnel_id=t.id AND needed.protocol IN('http','tls') AND needed.desired_state='active' AND needed.deleted_at IS NULL AND NOT EXISTS(SELECT 1 FROM paperboat.tunnel_domains d WHERE d.route_id=needed.id AND d.deleted_at IS NULL AND d.ownership_state='verified' AND d.conflict_state='clear')) FROM paperboat.tunnels t WHERE t.id=ANY($1::text[]) AND paperboat.workspace_edge_mode(t.account_id,paperboat.publication_workspace_team('tunnel',t.id))='self-hosted-only'`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, endpoint string
		var domainRequired bool
		if err := rows.Scan(&id, &endpoint, &domainRequired); err != nil {
			return err
		}
		i := index[id]
		tunnels[i].StableEndpoint = endpoint
		if domainRequired && tunnels[i].DesiredState == DesiredActive {
			tunnels[i].SummaryCode = "custom_domain_required"
		}
	}
	return rows.Err()
}
