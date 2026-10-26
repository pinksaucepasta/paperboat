package tunnelv1

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/planlimits"
	"github.com/pinksaucepasta/paperboat/server/internal/previewtunnelapi"
	"github.com/pinksaucepasta/paperboat/server/internal/publicationdns"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelcert"
)

func (r *SQLRepository) ListResourceRoutes(ctx context.Context, accountID, tunnelID string, after *ListPosition, limit int) ([]dbsqlc.TunnelRoute, error) {
	if strings.TrimSpace(accountID) == "" || strings.TrimSpace(tunnelID) == "" || limit < 1 || limit > previewtunnelapi.MaximumPageLimit+1 {
		return nil, ErrInvalidInput
	}
	createdAt, id := resourceAfter(after)
	rows, err := r.db.Queries().ListTunnelRoutesV1(ctx, dbsqlc.ListTunnelRoutesV1Params{
		AccountID: accountID, TunnelID: tunnelID, AfterCreatedAt: createdAt, AfterID: id, RowLimit: int32(limit),
	})
	return rows, translate(err)
}

func (r *SQLRepository) GetResourceRoute(ctx context.Context, accountID, tunnelID, routeID string) (dbsqlc.TunnelRoute, error) {
	if strings.TrimSpace(accountID) == "" || strings.TrimSpace(tunnelID) == "" || strings.TrimSpace(routeID) == "" {
		return dbsqlc.TunnelRoute{}, ErrInvalidInput
	}
	row, err := r.db.Queries().GetTunnelRouteV1(ctx, dbsqlc.GetTunnelRouteV1Params{
		RouteID: routeID, TunnelID: tunnelID, AccountID: accountID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return dbsqlc.TunnelRoute{}, ErrRouteNotFound
	}
	return row, translate(err)
}

func (r *SQLRepository) CreateResourceRoute(ctx context.Context, input RouteRecord) (ResourceMutationRecord, error) {
	if err := validateRouteRecord(input, false); err != nil {
		return ResourceMutationRecord{}, err
	}
	var result ResourceMutationRecord
	err := r.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := authorizeTunnelManagementTx(ctx, tx); err != nil {
			return err
		}
		q := tx.Queries()
		tunnel, err := q.GetTunnelForResourceV1(ctx, dbsqlc.GetTunnelForResourceV1Params{TunnelID: input.TunnelID, AccountID: input.AccountID})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return translate(err)
		}
		if tunnel.DesiredState == DesiredDeleted {
			return ErrTerminalState
		}
		candidate := routeCandidate(input)
		if err := rejectManagedHTTPForSelfhostOnly(ctx, tx, input.AccountID, candidate.Protocol, candidate.MatchType, input.RouteID); err != nil {
			return err
		}
		if err := validateEffectiveRouteCandidate(tunnel, candidate); err != nil {
			return err
		}
		op, replayed, err := beginResourceOperation(ctx, q, resourceOperationInput{
			ID: input.OperationID, AccountID: input.AccountID, IdempotencyKey: input.IdempotencyKey,
			RequestHash: input.RequestHash, OperationType: "route.create", ResourceKind: "route",
			ResourceID: input.RouteID, CorrelationID: input.CorrelationID,
		})
		if err != nil {
			return err
		}
		if replayed {
			result.Operation = op
			result.Replayed = true
			if op.ResourceID.Valid {
				result.Route, err = q.GetTunnelRouteV1(ctx, dbsqlc.GetTunnelRouteV1Params{RouteID: op.ResourceID.String, TunnelID: input.TunnelID, AccountID: input.AccountID})
				if errors.Is(err, pgx.ErrNoRows) {
					return ErrRouteNotFound
				}
				return translate(err)
			}
			return nil
		}
		conflict, err := q.HasConflictingTunnelRouteV1(ctx, dbsqlc.HasConflictingTunnelRouteV1Params{
			TunnelID: input.TunnelID, ExcludeID: input.RouteID, MatchType: candidate.MatchType,
			Protocol:   candidate.Protocol,
			PathPrefix: candidate.PathPrefix, MatchHostname: candidate.MatchHostname, WildcardSuffix: candidate.WildcardSuffix,
		})
		if err != nil {
			return translate(err)
		}
		if conflict {
			return ErrRouteConflict
		}
		if candidate.Protocol == "tcp" {
			if err := planlimits.CheckTx(ctx, tx, planlimits.Scope{Account: input.AccountID, Team: tunnel.OwnerTeamID.String}, planlimits.PublicTCP); err != nil {
				return err
			}
		}
		if candidate.Protocol == "tcp" || candidate.Protocol == "tls" {
			if err := q.LockTunnelPublicListenerAllocatorV1(ctx); err != nil {
				return translate(err)
			}
		}
		route, err := q.CreateTunnelRouteV1(ctx, createRouteParams(input))
		if errors.Is(err, pgx.ErrNoRows) && (candidate.Protocol == "tcp" || candidate.Protocol == "tls") {
			return ErrPublicListenerCapacity
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.ConstraintName == "tunnel_routes_public_listener_capacity_check" && (candidate.Protocol == "tcp" || candidate.Protocol == "tls") {
			return ErrPublicListenerCapacity
		}
		if err != nil {
			return translate(err)
		}
		updatedTunnel, err := q.BumpTunnelGenerationForResourceV1(ctx, dbsqlc.BumpTunnelGenerationForResourceV1Params{TunnelID: input.TunnelID, AccountID: input.AccountID, Now: input.Now})
		if err != nil {
			return translate(err)
		}
		if err := createResourceConfigGeneration(ctx, q, updatedTunnel, input.ActorID, input.Now); err != nil {
			return err
		}
		if err := syncEngine(ctx, tx, updatedTunnel); err != nil {
			return err
		}
		op, err = advanceResourceOperation(ctx, q, op.ID, route.ID, "connecting", 60, input.Now)
		if err != nil {
			return err
		}
		if err := recordResourceChange(ctx, q, resourceAuditInput{
			AuditEventID: input.AuditEventID, ParentAuditEventID: input.ParentAuditEventID,
			AccountID: input.AccountID, ActorID: auditActorID(input.ActorID, input.AuditActorID), ActorType: input.ActorType,
			EventType: "route.created", ParentEventType: "tunnel.route_created", ResourceType: "route", ResourceID: route.ID,
			TunnelID: input.TunnelID, OperationID: op.ID, IdempotencyKey: input.IdempotencyKey, RequestID: input.RequestID,
			CorrelationID: input.CorrelationID, SourceMachineID: input.SourceMachineID, Now: input.Now,
			Metadata: routeAuditMetadata(route),
			Message:  routeAuditMessage("route persisted and awaiting connector application", route),
		}); err != nil {
			return err
		}
		result = ResourceMutationRecord{Route: route, Operation: op, Changed: true}
		return nil
	})
	return result, translate(err)
}

func (r *SQLRepository) PatchResourceRoute(ctx context.Context, input RouteRecord) (ResourceMutationRecord, error) {
	if err := validateRouteRecord(input, true); err != nil {
		return ResourceMutationRecord{}, err
	}
	var result ResourceMutationRecord
	err := r.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := authorizeTunnelManagementTx(ctx, tx); err != nil {
			return err
		}
		q := tx.Queries()
		tunnel, err := q.GetTunnelForResourceV1(ctx, dbsqlc.GetTunnelForResourceV1Params{TunnelID: input.TunnelID, AccountID: input.AccountID})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return translate(err)
		}
		if tunnel.DesiredState == DesiredDeleted {
			return ErrTerminalState
		}
		op, replayed, err := beginResourceOperation(ctx, q, resourceOperationInput{
			ID: input.OperationID, AccountID: input.AccountID, IdempotencyKey: input.IdempotencyKey,
			RequestHash: input.RequestHash, OperationType: "route.patch", ResourceKind: "route",
			ResourceID: input.RouteID, CorrelationID: input.CorrelationID,
		})
		if err != nil {
			return err
		}
		current, err := q.GetTunnelRouteV1(ctx, dbsqlc.GetTunnelRouteV1Params{RouteID: input.RouteID, TunnelID: input.TunnelID, AccountID: input.AccountID})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRouteNotFound
		}
		if err != nil {
			return translate(err)
		}
		if replayed {
			result = ResourceMutationRecord{Route: current, Operation: op, Replayed: true, Changed: op.Outcome == "changed"}
			return nil
		}
		if current.DeletedAt.Valid {
			return ErrTerminalState
		}
		if current.Generation != input.ExpectedGeneration {
			return ErrGenerationConflict
		}
		candidate := routeCandidateFromRecord(current, input)
		if err := rejectManagedHTTPForSelfhostOnly(ctx, tx, input.AccountID, candidate.Protocol, candidate.MatchType, current.ID); err != nil {
			return err
		}
		if input.ProtocolSet && candidate.Protocol != current.Protocol && (candidate.Protocol == "tcp" || current.Protocol == "tcp" || candidate.Protocol == "tls" || current.Protocol == "tls") {
			return fmt.Errorf("%w: public TCP/TLS protocol cannot be changed in place; replace the route", ErrInvalidInput)
		}
		if err := validateEffectiveRouteCandidate(tunnel, candidate); err != nil {
			return err
		}
		if input.NameSet || input.ProtocolSet || input.MatchTypeSet || input.HostnameSet || input.WildcardSuffixSet || input.PathPrefixSet || input.OriginSet || input.PrioritySet || input.DesiredStateSet {
			conflict, conflictErr := q.HasConflictingTunnelRouteV1(ctx, dbsqlc.HasConflictingTunnelRouteV1Params{
				TunnelID: input.TunnelID, ExcludeID: current.ID, MatchType: candidate.MatchType,
				Protocol:   candidate.Protocol,
				PathPrefix: candidate.PathPrefix, MatchHostname: candidate.MatchHostname, WildcardSuffix: candidate.WildcardSuffix,
			})
			if conflictErr != nil {
				return translate(conflictErr)
			}
			if conflict {
				return ErrRouteConflict
			}
		}
		changed := routeRecordChanges(current, input)
		now := input.Now
		if changed {
			updated, updateErr := q.UpdateTunnelRouteV1(ctx, updateRouteParams(input))
			if errors.Is(updateErr, pgx.ErrNoRows) {
				return ErrGenerationConflict
			}
			if updateErr != nil {
				return translate(updateErr)
			}
			current = updated
			tunnel, err = q.BumpTunnelGenerationForResourceV1(ctx, dbsqlc.BumpTunnelGenerationForResourceV1Params{TunnelID: input.TunnelID, AccountID: input.AccountID, Now: now})
			if err != nil {
				return translate(err)
			}
			if err := createResourceConfigGeneration(ctx, q, tunnel, input.ActorID, now); err != nil {
				return err
			}
			if err := syncEngine(ctx, tx, tunnel); err != nil {
				return err
			}
		}
		outcome := "unchanged"
		if changed {
			outcome = "changed"
		}
		if changed {
			op, err = advanceResourceOperation(ctx, q, op.ID, current.ID, "connecting", 60, now)
		} else {
			op, err = completeResourceOperation(ctx, q, op.ID, current.ID, "ready", outcome, now)
		}
		if err != nil {
			return err
		}
		if err := recordResourceChange(ctx, q, resourceAuditInput{
			AuditEventID: input.AuditEventID, ParentAuditEventID: input.ParentAuditEventID,
			AccountID: input.AccountID, ActorID: auditActorID(input.ActorID, input.AuditActorID), ActorType: input.ActorType,
			EventType: "route.updated", ParentEventType: "tunnel.route_updated", ResourceType: "route", ResourceID: current.ID,
			TunnelID: input.TunnelID, OperationID: op.ID, IdempotencyKey: input.IdempotencyKey, RequestID: input.RequestID,
			CorrelationID: input.CorrelationID, SourceMachineID: input.SourceMachineID, Now: now,
			Metadata: routeAuditMetadata(current),
			Message:  routeAuditMessage("route update persisted", current),
		}); err != nil {
			return err
		}
		result = ResourceMutationRecord{Route: current, Operation: op, Changed: changed}
		return nil
	})
	return result, translate(err)
}

func (r *SQLRepository) DeleteResourceRoute(ctx context.Context, input RouteRecord) (ResourceMutationRecord, error) {
	if err := validateRouteRecord(input, true); err != nil {
		return ResourceMutationRecord{}, err
	}
	var result ResourceMutationRecord
	err := r.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := authorizeTunnelManagementTx(ctx, tx); err != nil {
			return err
		}
		q := tx.Queries()
		_, err := q.GetTunnelForResourceV1(ctx, dbsqlc.GetTunnelForResourceV1Params{TunnelID: input.TunnelID, AccountID: input.AccountID})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return translate(err)
		}
		op, replayed, err := beginResourceOperation(ctx, q, resourceOperationInput{
			ID: input.OperationID, AccountID: input.AccountID, IdempotencyKey: input.IdempotencyKey,
			RequestHash: input.RequestHash, OperationType: "route.delete", ResourceKind: "route",
			ResourceID: input.RouteID, CorrelationID: input.CorrelationID,
		})
		if err != nil {
			return err
		}
		current, err := q.GetTunnelRouteV1(ctx, dbsqlc.GetTunnelRouteV1Params{RouteID: input.RouteID, TunnelID: input.TunnelID, AccountID: input.AccountID})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRouteNotFound
		}
		if err != nil {
			return translate(err)
		}
		if replayed {
			result = ResourceMutationRecord{Route: current, Operation: op, Replayed: true, Changed: op.Outcome == "changed"}
			return nil
		}
		if current.DeletedAt.Valid {
			op, err = completeResourceOperation(ctx, q, op.ID, current.ID, "ready", "unchanged", input.Now)
			if err != nil {
				return err
			}
			result = ResourceMutationRecord{Route: current, Operation: op, Changed: false}
			return nil
		}
		if current.Generation != input.ExpectedGeneration {
			return ErrGenerationConflict
		}
		current, err = q.DeleteTunnelRouteV1(ctx, dbsqlc.DeleteTunnelRouteV1Params{Now: sql.NullTime{Time: input.Now, Valid: true}, ActorID: input.ActorID, RouteID: input.RouteID, TunnelID: input.TunnelID, ExpectedGeneration: input.ExpectedGeneration})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrGenerationConflict
		}
		if err != nil {
			return translate(err)
		}
		tunnel, err := q.BumpTunnelGenerationForResourceV1(ctx, dbsqlc.BumpTunnelGenerationForResourceV1Params{TunnelID: input.TunnelID, AccountID: input.AccountID, Now: input.Now})
		if err != nil {
			return translate(err)
		}
		if err := createResourceConfigGeneration(ctx, q, tunnel, input.ActorID, input.Now); err != nil {
			return err
		}
		if err := syncEngine(ctx, tx, tunnel); err != nil {
			return err
		}
		op, err = advanceResourceOperation(ctx, q, op.ID, current.ID, "draining", 60, input.Now)
		if err != nil {
			return err
		}
		if err := recordResourceChange(ctx, q, resourceAuditInput{
			AuditEventID: input.AuditEventID, ParentAuditEventID: input.ParentAuditEventID,
			AccountID: input.AccountID, ActorID: auditActorID(input.ActorID, input.AuditActorID), ActorType: input.ActorType,
			EventType: "route.deleted", ParentEventType: "tunnel.route_deleted", ResourceType: "route", ResourceID: current.ID,
			TunnelID: input.TunnelID, OperationID: op.ID, IdempotencyKey: input.IdempotencyKey, RequestID: input.RequestID,
			CorrelationID: input.CorrelationID, SourceMachineID: input.SourceMachineID, Now: input.Now,
			Metadata: map[string]any{"generation": current.Generation}, Message: "route deletion persisted; connector drain pending",
		}); err != nil {
			return err
		}
		result = ResourceMutationRecord{Route: current, Operation: op, Changed: true}
		return nil
	})
	return result, translate(err)
}

func createRouteParams(input RouteRecord) dbsqlc.CreateTunnelRouteV1Params {
	tlsVerification, serverName, caRef, clientRef := routeTLSValues(input.Origin)
	return dbsqlc.CreateTunnelRouteV1Params{
		ID: input.RouteID, TunnelID: input.TunnelID, Name: input.Name, Protocol: input.Protocol,
		MatchType: input.MatchType, MatchHostname: input.Hostname, WildcardSuffix: input.WildcardSuffix,
		PathPrefix: input.PathPrefix, Priority: input.Priority, OriginScheme: input.Origin.Scheme,
		OriginAddress: input.Origin.Address, PreserveHost: input.Origin.PreserveHost, HostOverride: nullableStringPtr(input.Origin.HostOverride),
		TlsVerification: tlsVerification, TlsServerName: nullableStringPtr(serverName), CaReference: nullableStringPtr(caRef),
		MtlsCredentialReference: nullableStringPtr(clientRef), ConnectTimeoutMs: input.ConnectTimeoutMS, IdleTimeoutMs: input.IdleTimeoutMS,
		MaxConcurrentStreams: input.MaxConcurrentStreams, DesiredState: "active", CreatedByActorID: input.ActorID, UpdatedByActorID: input.ActorID, Now: input.Now,
		PublicTcpListenerID: nullableString(input.PublicTCPListenerID), PublicTcpPortMin: input.PublicTCPPortMin, PublicTcpPortMax: input.PublicTCPPortMax,
	}
}

func updateRouteParams(input RouteRecord) dbsqlc.UpdateTunnelRouteV1Params {
	tlsVerification, serverName, caRef, clientRef := routeTLSValues(input.Origin)
	return dbsqlc.UpdateTunnelRouteV1Params{
		NameSet: input.NameSet, Name: input.Name, ProtocolSet: input.ProtocolSet, Protocol: input.Protocol,
		MatchTypeSet: input.MatchTypeSet, MatchType: input.MatchType, MatchHostnameSet: input.HostnameSet, MatchHostname: input.Hostname,
		WildcardSuffixSet: input.WildcardSuffixSet, WildcardSuffix: input.WildcardSuffix, PathPrefixSet: input.PathPrefixSet, PathPrefix: input.PathPrefix,
		PrioritySet: input.PrioritySet, Priority: input.Priority, OriginSchemeSet: input.OriginSet, OriginScheme: input.Origin.Scheme,
		OriginAddressSet: input.OriginSet, OriginAddress: input.Origin.Address, PreserveHostSet: input.OriginSet, PreserveHost: input.Origin.PreserveHost,
		HostOverrideSet: input.OriginSet, HostOverride: nullableStringPtr(input.Origin.HostOverride), TlsVerificationSet: input.OriginSet, TlsVerification: tlsVerification,
		TlsServerNameSet: input.OriginSet, TlsServerName: nullableStringPtr(serverName), CaReferenceSet: input.OriginSet, CaReference: nullableStringPtr(caRef),
		MtlsCredentialReferenceSet: input.OriginSet, MtlsCredentialReference: nullableStringPtr(clientRef), DesiredStateSet: input.DesiredStateSet,
		DesiredState: input.DesiredState, UpdatedByActorID: input.ActorID, Now: input.Now, RouteID: input.RouteID, TunnelID: input.TunnelID,
		ExpectedGeneration: input.ExpectedGeneration,
		ConnectTimeoutSet:  input.ConnectTimeoutSet, ConnectTimeoutMs: input.ConnectTimeoutMS,
		IdleTimeoutSet: input.IdleTimeoutSet, IdleTimeoutMs: input.IdleTimeoutMS,
		MaxStreamsSet: input.MaxStreamsSet, MaxConcurrentStreams: input.MaxConcurrentStreams,
	}
}

func nullableStringPtr(value *string) sql.NullString {
	if value == nil {
		return sql.NullString{}
	}
	return nullableString(*value)
}

func resourceAfter(after *ListPosition) (sql.NullTime, sql.NullString) {
	if after == nil {
		return sql.NullTime{}, sql.NullString{}
	}
	return sql.NullTime{Time: after.CreatedAt.UTC(), Valid: true}, nullableString(after.ID)
}

func routeTLSValues(origin RouteOriginRequest) (string, *string, *string, *string) {
	if origin.Scheme != "https" || origin.TLS == nil {
		if origin.Scheme == "https" {
			return "system", nil, nil, nil
		}
		return "not_applicable", nil, nil, nil
	}
	return origin.TLS.Verification, origin.TLS.ServerName, origin.TLS.CAReference, origin.TLS.ClientCredentialReference
}

func routeCandidate(input RouteRecord) dbsqlc.TunnelRoute {
	tlsVerification, serverName, caReference, clientReference := routeTLSNullValues(input.Origin)
	return dbsqlc.TunnelRoute{ID: input.RouteID, TunnelID: input.TunnelID, Name: input.Name, Protocol: input.Protocol, MatchType: input.MatchType,
		MatchHostname: input.Hostname, WildcardSuffix: input.WildcardSuffix, PathPrefix: input.PathPrefix, Priority: input.Priority,
		OriginScheme: input.Origin.Scheme, OriginAddress: input.Origin.Address, PreserveHost: input.Origin.PreserveHost,
		TlsVerification: tlsVerification, TlsServerName: serverName, CaReference: caReference, MtlsCredentialReference: clientReference,
		ConnectTimeoutMs: input.ConnectTimeoutMS, IdleTimeoutMs: input.IdleTimeoutMS, MaxConcurrentStreams: input.MaxConcurrentStreams,
		DesiredState: input.DesiredState}
}

func routeCandidateFromRecord(current dbsqlc.TunnelRoute, input RouteRecord) dbsqlc.TunnelRoute {
	candidate := current
	if input.NameSet {
		candidate.Name = input.Name
	}
	if input.ProtocolSet {
		candidate.Protocol = input.Protocol
	}
	if input.MatchTypeSet {
		candidate.MatchType = input.MatchType
	}
	if input.HostnameSet {
		candidate.MatchHostname = input.Hostname
	}
	if input.WildcardSuffixSet {
		candidate.WildcardSuffix = input.WildcardSuffix
	}
	if input.PathPrefixSet {
		candidate.PathPrefix = input.PathPrefix
	}
	if input.OriginSet {
		candidate.OriginScheme = input.Origin.Scheme
		candidate.OriginAddress = input.Origin.Address
		candidate.PreserveHost = input.Origin.PreserveHost
		candidate.HostOverride = nullableStringPtr(input.Origin.HostOverride)
		candidate.TlsVerification, candidate.TlsServerName, candidate.CaReference, candidate.MtlsCredentialReference = routeTLSNullValues(input.Origin)
	}
	if input.PrioritySet {
		candidate.Priority = input.Priority
	}
	if input.ConnectTimeoutSet {
		candidate.ConnectTimeoutMs = input.ConnectTimeoutMS
	}
	if input.IdleTimeoutSet {
		candidate.IdleTimeoutMs = input.IdleTimeoutMS
	}
	if input.MaxStreamsSet {
		candidate.MaxConcurrentStreams = input.MaxConcurrentStreams
	}
	if input.DesiredStateSet {
		candidate.DesiredState = input.DesiredState
	}
	return candidate
}

// validateEffectiveRouteCandidate validates the merged post-mutation route,
// rather than only fields present in a PATCH body. This keeps protocol,
// tunnel-access, host-match, and origin semantics atomic across partial
// updates.
func validateEffectiveRouteCandidate(tunnel dbsqlc.Tunnel, candidate dbsqlc.TunnelRoute) error {
	if candidate.Protocol == "tcp" {
		if tunnel.AccessMode != AccessPublic || candidate.OriginScheme != "tcp" || candidate.MatchType != "managed" || !candidate.MatchHostname.Valid || candidate.PathPrefix.Valid || candidate.PreserveHost || candidate.HostOverride.Valid {
			return fmt.Errorf("%w: public TCP routes require a public tunnel, managed hostname, tcp origin, and no path", ErrInvalidInput)
		}
	}
	if candidate.Protocol == "tls" {
		if tunnel.AccessMode != AccessPublic || candidate.OriginScheme != "tcp" || candidate.MatchType != "managed" || !candidate.MatchHostname.Valid || candidate.WildcardSuffix.Valid || candidate.PathPrefix.Valid || candidate.PreserveHost || candidate.HostOverride.Valid || candidate.TlsVerification != "not_applicable" {
			return fmt.Errorf("%w: TLS routes require a public tunnel, exact managed hostname, tcp origin, and no HTTP path/header settings", ErrInvalidInput)
		}
		endpoint, err := url.Parse(tunnel.StableEndpoint)
		if err != nil || endpoint.Hostname() == "" || candidate.MatchHostname.String != strings.ToLower(endpoint.Hostname()) {
			return fmt.Errorf("%w: TLS managed hostname must equal the tunnel's authoritative endpoint", ErrInvalidInput)
		}
	}
	if candidate.Protocol == "private_tcp" {
		if tunnel.AccessMode != AccessPrivate || candidate.OriginScheme != "tcp" {
			return fmt.Errorf("%w: tcp_private routes require a private tunnel and tcp origin", ErrInvalidInput)
		}
		if candidate.MatchType != "catch_all" || candidate.MatchHostname.Valid || candidate.WildcardSuffix.Valid || candidate.PathPrefix.Valid {
			return fmt.Errorf("%w: tcp_private routes require a catch-all host match without a path prefix", ErrInvalidInput)
		}
	}
	if candidate.Protocol == "http" && candidate.OriginScheme == "tcp" {
		return fmt.Errorf("%w: HTTP routes cannot target a tcp origin", ErrInvalidInput)
	}
	// The edge serves a public HTTP route only for a concrete hostname, so a
	// public catch-all route would be accepted here and never answer.
	if candidate.Protocol == "http" && candidate.MatchType == "catch_all" && tunnel.AccessMode == AccessPublic {
		return fmt.Errorf("%w: public HTTP routes require an exact, wildcard or managed hostname; catch-all is only valid on private tunnels", ErrInvalidInput)
	}
	if candidate.MatchType == "catch_all" && (candidate.MatchHostname.Valid || candidate.WildcardSuffix.Valid) {
		return fmt.Errorf("%w: catch-all routes cannot include a hostname", ErrInvalidInput)
	}
	return nil
}

func nullStringValue(value sql.NullString) string {
	if value.Valid {
		return value.String
	}
	return ""
}

func routeRecordChanges(current dbsqlc.TunnelRoute, input RouteRecord) bool {
	candidate := routeCandidateFromRecord(current, input)
	return current.Name != candidate.Name || current.Protocol != candidate.Protocol || current.MatchType != candidate.MatchType ||
		current.MatchHostname != candidate.MatchHostname || current.WildcardSuffix != candidate.WildcardSuffix || current.PathPrefix != candidate.PathPrefix ||
		current.Priority != candidate.Priority || current.OriginScheme != candidate.OriginScheme || current.OriginAddress != candidate.OriginAddress ||
		current.PreserveHost != candidate.PreserveHost || current.HostOverride != candidate.HostOverride || current.TlsVerification != candidate.TlsVerification ||
		current.TlsServerName != candidate.TlsServerName || current.CaReference != candidate.CaReference || current.MtlsCredentialReference != candidate.MtlsCredentialReference ||
		current.ConnectTimeoutMs != candidate.ConnectTimeoutMs || current.IdleTimeoutMs != candidate.IdleTimeoutMs || current.MaxConcurrentStreams != candidate.MaxConcurrentStreams ||
		current.DesiredState != candidate.DesiredState
}

func routeTLSNullValues(origin RouteOriginRequest) (string, sql.NullString, sql.NullString, sql.NullString) {
	verification, serverName, caReference, clientReference := routeTLSValues(origin)
	return verification, nullableStringPtr(serverName), nullableStringPtr(caReference), nullableStringPtr(clientReference)
}

func routeAuditMetadata(route dbsqlc.TunnelRoute) map[string]any {
	return map[string]any{
		"generation": route.Generation, "protocol": wireProtocol(route.Protocol), "origin_scheme": route.OriginScheme,
		"desired_state": route.DesiredState, "origin_tls_verification": route.TlsVerification,
		"origin_tls_insecure_development": route.TlsVerification == "insecure_development",
	}
}

func routeAuditMessage(message string, route dbsqlc.TunnelRoute) string {
	if route.TlsVerification == "insecure_development" {
		return message + "; warning: origin TLS certificate verification is disabled for development"
	}
	return message
}

func createResourceConfigGeneration(ctx context.Context, q *dbsqlc.Queries, tunnel dbsqlc.Tunnel, actorID string, now time.Time) error {
	if tunnel.Generation > 1 {
		if _, err := q.GetTunnelConfigGenerationV1(ctx, dbsqlc.GetTunnelConfigGenerationV1Params{TunnelID: tunnel.ID, Generation: tunnel.Generation - 1}); errors.Is(err, pgx.ErrNoRows) {
			return ErrConfigGenerationChain
		} else if err != nil {
			return translate(err)
		}
	}
	routes, err := q.ListActiveTunnelRoutesForSnapshotV1(ctx, tunnel.ID)
	if err != nil {
		return translate(err)
	}
	snapshot, err := resourceConfigSnapshot(tunnel, routes)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(snapshot)
	previous := sql.NullInt64{Int64: tunnel.Generation - 1, Valid: tunnel.Generation > 1}
	_, err = q.CreatePreviewTunnelConfigGeneration(ctx, dbsqlc.CreatePreviewTunnelConfigGenerationParams{
		TunnelID: tunnel.ID, Generation: tunnel.Generation, PreviousGeneration: previous, ContentHash: hash[:], Snapshot: snapshot,
		ActivationState: "pending", CreatedByActorID: actorID, Now: now, RetainedUntil: now.Add(configGenerationRetention),
	})
	return translate(err)
}

func resourceConfigSnapshot(tunnel dbsqlc.Tunnel, routes []dbsqlc.TunnelRoute) ([]byte, error) {
	var expiresAt *time.Time
	if tunnel.ExpiresAt.Valid {
		value := tunnel.ExpiresAt.Time.UTC()
		expiresAt = &value
	}
	items := make([]initialTunnelRouteSnapshot, 0, len(routes))
	for _, route := range routes {
		items = append(items, snapshotRoute(route))
	}
	encoded, err := json.Marshal(initialTunnelConfigSnapshot{Schema: Schema, Kind: "tunnel_config_snapshot", TunnelID: tunnel.ID, Generation: tunnel.Generation, Name: tunnel.Name, DesiredState: tunnel.DesiredState,
		AccessMode: tunnel.AccessMode, StableEndpointID: tunnel.StableEndpointID, StableEndpoint: tunnel.StableEndpoint, ExpiresAt: expiresAt, Routes: items})
	if err != nil {
		return nil, err
	}
	return canonicalConfigSnapshot(encoded)
}

type resourceOperationInput struct {
	ID             string
	AccountID      string
	IdempotencyKey string
	RequestHash    [sha256.Size]byte
	OperationType  string
	ResourceKind   string
	ResourceID     string
	CorrelationID  string
}

func beginResourceOperation(ctx context.Context, q *dbsqlc.Queries, input resourceOperationInput) (dbsqlc.Operation, bool, error) {
	op, err := q.CreatePreviewTunnelOperation(ctx, dbsqlc.CreatePreviewTunnelOperationParams{
		ID: input.ID, AccountID: input.AccountID, IdempotencyKey: input.IdempotencyKey, RequestHash: input.RequestHash[:],
		OperationType: input.OperationType, ResourceKind: input.ResourceKind, ResourceID: nullableString(input.ResourceID),
		Phase: "persisting", State: "running", Progress: 20, Outcome: "unchanged", CorrelationID: input.CorrelationID,
	})
	if err == nil {
		return op, false, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return dbsqlc.Operation{}, false, translate(err)
	}
	existing, getErr := q.GetPreviewTunnelOperationByIdempotency(ctx, dbsqlc.GetPreviewTunnelOperationByIdempotencyParams{AccountID: input.AccountID, IdempotencyKey: input.IdempotencyKey})
	if getErr != nil {
		return dbsqlc.Operation{}, false, translate(getErr)
	}
	// Creation generates a candidate identity on each request. The persisted
	// operation owns the identity after its first commit; mutation targets
	// remain exact resource fences. Account lookup, kind, type and request hash
	// still bind replay, and callers load the result under its original parent.
	generatedResource := input.OperationType == "route.create" && input.ResourceKind == "route" || input.OperationType == "domain.create" && input.ResourceKind == "domain_binding" || (input.OperationType == "connector.enrollment.issue" || input.OperationType == "connector.enrollment.exchange") && input.ResourceKind == "connector"
	if existing.OperationType != input.OperationType || existing.ResourceKind != input.ResourceKind || !existing.ResourceID.Valid || (!generatedResource && existing.ResourceID.String != input.ResourceID) || !bytes.Equal(existing.RequestHash, input.RequestHash[:]) {
		return dbsqlc.Operation{}, false, ErrIdempotencyConflict
	}
	return existing, true, nil
}

func completeResourceOperation(ctx context.Context, q *dbsqlc.Queries, operationID, resourceID, phase, outcome string, now time.Time) (dbsqlc.Operation, error) {
	op, err := q.CompletePreviewTunnelOperation(ctx, dbsqlc.CompletePreviewTunnelOperationParams{ResourceID: nullableString(resourceID), Phase: phase,
		State: "succeeded", Progress: 100, Outcome: outcome, ResultReference: nullableString(resourceID), UpdatedAt: now.UTC(),
		CompletedAt: sql.NullTime{Time: now.UTC(), Valid: true}, ID: operationID})
	return op, translate(err)
}

func advanceResourceOperation(ctx context.Context, q *dbsqlc.Queries, operationID, resourceID, phase string, progress int16, now time.Time) (dbsqlc.Operation, error) {
	op, err := q.CompletePreviewTunnelOperation(ctx, dbsqlc.CompletePreviewTunnelOperationParams{ResourceID: nullableString(resourceID), Phase: phase,
		State: "running", Progress: progress, Outcome: "changed", ResultReference: sql.NullString{}, UpdatedAt: now.UTC(), CompletedAt: sql.NullTime{}, ID: operationID})
	return op, translate(err)
}

type resourceAuditInput struct {
	AuditEventID       string
	ParentAuditEventID string
	AccountID          string
	ActorID            string
	ActorType          string
	EventType          string
	ParentEventType    string
	ResourceType       string
	ResourceID         string
	TunnelID           string
	OperationID        string
	IdempotencyKey     string
	RequestID          string
	CorrelationID      string
	SourceMachineID    string
	Now                time.Time
	Metadata           map[string]any
	Message            string
}

func recordResourceChange(ctx context.Context, q *dbsqlc.Queries, input resourceAuditInput) error {
	if err := insertResourceAudit(ctx, q, input.AuditEventID, input.AccountID, input.ActorID, input.ActorType, input.EventType, "update", "changed", input.ResourceType, input.ResourceID,
		input.IdempotencyKey, input.RequestID, input.CorrelationID, input.SourceMachineID, input.Now, input.Metadata); err != nil {
		return err
	}
	if input.ParentAuditEventID != "" {
		metadata := map[string]any{"resource_kind": input.ResourceType, "resource_id": input.ResourceID}
		for key, value := range input.Metadata {
			metadata[key] = value
		}
		if err := insertResourceAudit(ctx, q, input.ParentAuditEventID, input.AccountID, input.ActorID, input.ActorType, input.ParentEventType, "update", "changed", "tunnel", input.TunnelID,
			input.IdempotencyKey, input.RequestID, input.CorrelationID, input.SourceMachineID, input.Now, metadata); err != nil {
			return err
		}
	}
	return insertResourceLog(ctx, q, input)
}

func insertResourceAudit(ctx context.Context, q *dbsqlc.Queries, id, accountID, actorID, actorType, eventType, changeType, outcome, resourceType, resourceID, idempotencyKey, requestID, correlationID, sourceMachineID string, createdAt time.Time, metadata map[string]any) error {
	safe, err := previewtunnelapi.SafeMetadata(metadata)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(safe)
	if err != nil {
		return fmt.Errorf("encode resource audit metadata: %w", err)
	}
	_, err = q.InsertPreviewTunnelAuditEvent(ctx, dbsqlc.InsertPreviewTunnelAuditEventParams{
		ID: id, AccountID: nullableString(accountID), ActorID: nullableString(actorID), ActorUserID: actorUserID(actorType, actorID), ActorType: actorType,
		EventType: eventType, ChangeType: changeType, Outcome: outcome, ResourceType: resourceType, ResourceID: resourceID,
		IdempotencyKey: nullableString(idempotencyKey), RequestID: nullableString(requestID), CorrelationID: nullableString(correlationID), SourceMachineID: nullableString(sourceMachineID), Metadata: encoded, CreatedAt: createdAt.UTC(),
	})
	return translate(err)
}

func insertResourceLog(ctx context.Context, q *dbsqlc.Queries, input resourceAuditInput) error {
	if err := validateLogText(input.Message, maxResourceLogMessageBytes); err != nil {
		return err
	}
	if err := validateLogCorrelation(input.CorrelationID); err != nil {
		return err
	}
	safe, err := previewtunnelapi.SafeMetadata(input.Metadata)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(safe)
	if err != nil {
		return err
	}
	code := strings.ReplaceAll(input.EventType, ".", "_")
	_, err = q.CreateTunnelLogEntryV1(ctx, dbsqlc.CreateTunnelLogEntryV1Params{ID: "log_" + input.AuditEventID, AccountID: input.AccountID, TunnelID: nullableString(input.TunnelID),
		Level: "info", Component: "control", Code: code, Message: input.Message, Metadata: encoded, CorrelationID: input.CorrelationID, OccurredAt: input.Now.UTC()})
	return translate(err)
}

func validateRouteRecord(input RouteRecord, requireGeneration bool) error {
	for name, value := range map[string]string{"operation_id": input.OperationID, "audit_event_id": input.AuditEventID, "account_id": input.AccountID, "tunnel_id": input.TunnelID, "route_id": input.RouteID, "idempotency_key": input.IdempotencyKey, "actor_id": input.ActorID, "actor_type": input.ActorType, "correlation_id": input.CorrelationID} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%w: %s is required", ErrInvalidInput, name)
		}
	}
	if requireGeneration && input.ExpectedGeneration < 1 {
		return fmt.Errorf("%w: expected generation is required", ErrInvalidInput)
	}
	if input.RequestHash == ([sha256.Size]byte{}) {
		return fmt.Errorf("%w: request hash is required", ErrInvalidInput)
	}
	if !validActorType(input.ActorType) {
		return fmt.Errorf("%w: actor type is invalid", ErrInvalidInput)
	}
	return nil
}

func (r *SQLRepository) ListResourceDomains(ctx context.Context, accountID, tunnelID string, after *ListPosition, limit int) ([]dbsqlc.TunnelDomain, error) {
	if strings.TrimSpace(accountID) == "" || strings.TrimSpace(tunnelID) == "" || limit < 1 || limit > previewtunnelapi.MaximumPageLimit+1 {
		return nil, ErrInvalidInput
	}
	createdAt, id := resourceAfter(after)
	rows, err := r.db.Queries().ListTunnelDomainsV1(ctx, dbsqlc.ListTunnelDomainsV1Params{AccountID: accountID, TunnelID: tunnelID, AfterCreatedAt: createdAt, AfterID: id, RowLimit: int32(limit)})
	return rows, translate(err)
}

func (r *SQLRepository) GetResourceDomain(ctx context.Context, accountID, tunnelID, domainID string) (dbsqlc.TunnelDomain, error) {
	if strings.TrimSpace(accountID) == "" || strings.TrimSpace(tunnelID) == "" || strings.TrimSpace(domainID) == "" {
		return dbsqlc.TunnelDomain{}, ErrInvalidInput
	}
	row, err := r.db.Queries().GetTunnelDomainV1(ctx, dbsqlc.GetTunnelDomainV1Params{DomainID: domainID, AccountID: accountID, TunnelID: tunnelID})
	if errors.Is(err, pgx.ErrNoRows) {
		return dbsqlc.TunnelDomain{}, ErrDomainNotFound
	}
	return row, translate(err)
}

func (r *SQLRepository) CreateResourceDomain(ctx context.Context, input DomainRecord) (ResourceMutationRecord, error) {
	if err := validateDomainRecord(input, false); err != nil {
		return ResourceMutationRecord{}, err
	}
	var result ResourceMutationRecord
	err := r.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := authorizeTunnelManagementTx(ctx, tx); err != nil {
			return err
		}
		q := tx.Queries()
		tunnel, err := q.GetTunnelForResourceV1(ctx, dbsqlc.GetTunnelForResourceV1Params{TunnelID: input.TunnelID, AccountID: input.AccountID})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return translate(err)
		}
		if tunnel.DesiredState == DesiredDeleted {
			return ErrTerminalState
		}
		route, err := q.GetTunnelRouteV1(ctx, dbsqlc.GetTunnelRouteV1Params{RouteID: input.RouteID, TunnelID: input.TunnelID, AccountID: input.AccountID})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRouteNotFound
		}
		if err != nil {
			return translate(err)
		}
		if route.DeletedAt.Valid || route.DesiredState == "deleted" {
			return ErrRouteNotFound
		}
		var allowed bool
		if err := tx.QueryRow(ctx, `SELECT paperboat.workspace_edge_mode(account_id,owner_team_id)='managed-only' OR paperboat.selfhost_publication_hostname_allowed($3) FROM paperboat.tunnels WHERE id=$1 AND account_id=$2`, input.TunnelID, input.AccountID, input.Hostname).Scan(&allowed); err != nil {
			return err
		}
		if !allowed {
			return fmt.Errorf("%w: self-hosted publications require a customer domain outside Paperboat-managed namespaces", ErrInvalidInput)
		}
		op, replayed, err := beginResourceOperation(ctx, q, resourceOperationInput{ID: input.OperationID, AccountID: input.AccountID, IdempotencyKey: input.IdempotencyKey, RequestHash: input.RequestHash, OperationType: "domain.create", ResourceKind: "domain_binding", ResourceID: input.DomainID, CorrelationID: input.CorrelationID})
		if err != nil {
			return err
		}
		if replayed {
			result.Operation = op
			result.Replayed = true
			if op.ResourceID.Valid {
				result.Domain, err = q.GetTunnelDomainV1(ctx, dbsqlc.GetTunnelDomainV1Params{DomainID: op.ResourceID.String, AccountID: input.AccountID, TunnelID: input.TunnelID})
				if errors.Is(err, pgx.ErrNoRows) {
					return ErrDomainNotFound
				}
				return translate(err)
			}
			return nil
		}
		if existing, lookupErr := q.GetTunnelDomainByHostnameV1(ctx, input.Hostname); lookupErr == nil {
			if err := domainClaimError(existing, input.Now); err != nil {
				return err
			}
		} else if !errors.Is(lookupErr, pgx.ErrNoRows) {
			return translate(lookupErr)
		}
		targets, err := publicationdns.ResolveTargetsTx(ctx, tx, "tunnel", input.TunnelID, tunnel.StableEndpoint)
		if err != nil {
			return err
		}

		input.DNSTarget = targets[0]
		if input.DNSProvider == "" {
			input.DNSProvider = "generic"
		}
		if input.CertificateStrategy == "" {
			input.CertificateStrategy = "managed"
		}
		if _, err := normalizeDomainCertificateStrategy(input.CertificateStrategy, input.MatchType, route.Protocol); err != nil {
			return err
		}
		{
			input.ExpectedRecords, err = json.Marshal(publicationdns.Records(targets, input.Hostname, input.DNSProvider))
			if err != nil {
				return err
			}
		}
		domain, err := q.CreateTunnelDomainV1(ctx, dbsqlc.CreateTunnelDomainV1Params{ID: input.DomainID, AccountID: input.AccountID, TunnelID: input.TunnelID, RouteID: input.RouteID,
			Hostname: input.Hostname, MatchType: input.MatchType, OwnershipChallengeReference: input.ChallengeReference, DnsTarget: input.DNSTarget,
			ObservedRecords: []byte("[]"), CertificateStrategy: input.CertificateStrategy, DnsProvider: input.DNSProvider, ExpectedRecords: input.ExpectedRecords, Now: input.Now})
		if err != nil {
			return translate(err)
		}
		op, err = advanceResourceOperation(ctx, q, op.ID, domain.ID, "waiting_for_dns", 35, input.Now)
		if err != nil {
			return err
		}
		if err := recordResourceChange(ctx, q, resourceAuditInput{AuditEventID: input.AuditEventID, ParentAuditEventID: input.ParentAuditEventID, AccountID: input.AccountID,
			ActorID: auditActorID(input.ActorID, input.AuditActorID), ActorType: input.ActorType, EventType: "domain.created", ParentEventType: "tunnel.domain_created",
			ResourceType: "domain_binding", ResourceID: domain.ID, TunnelID: input.TunnelID, OperationID: op.ID, IdempotencyKey: input.IdempotencyKey,
			RequestID: input.RequestID, CorrelationID: input.CorrelationID, SourceMachineID: input.SourceMachineID, Now: input.Now,
			Metadata: map[string]any{"hostname": input.Hostname, "state": "waiting_dns"}, Message: "domain binding persisted; DNS ownership proof pending"}); err != nil {
			return err
		}
		result = ResourceMutationRecord{Domain: domain, Operation: op, Changed: true}
		return nil
	})
	return result, translate(err)
}

// domainClaimError says why a hostname cannot be bound: another binding holds it, or it was removed
// recently and is quarantined. Nil when it can be claimed.
func domainClaimError(existing dbsqlc.TunnelDomain, now time.Time) error {
	switch {
	case !existing.DeletedAt.Valid:
		return ErrDomainConflict
	case existing.QuarantineUntil.Valid && existing.QuarantineUntil.Time.After(now):
		return fmt.Errorf("%w: %w", ErrDomainConflict, ErrDomainQuarantined)
	}
	return nil
}

func (r *SQLRepository) DeleteResourceDomain(ctx context.Context, input DomainRecord) (ResourceMutationRecord, error) {
	if err := validateDomainRecord(input, true); err != nil {
		return ResourceMutationRecord{}, err
	}
	var result ResourceMutationRecord
	err := r.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := authorizeTunnelManagementTx(ctx, tx); err != nil {
			return err
		}
		q := tx.Queries()
		tunnel, err := q.GetTunnelForResourceV1(ctx, dbsqlc.GetTunnelForResourceV1Params{TunnelID: input.TunnelID, AccountID: input.AccountID})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return translate(err)
		}
		if tunnel.DesiredState == DesiredDeleted {
			return ErrTerminalState
		}
		op, replayed, err := beginResourceOperation(ctx, q, resourceOperationInput{ID: input.OperationID, AccountID: input.AccountID, IdempotencyKey: input.IdempotencyKey, RequestHash: input.RequestHash, OperationType: "domain.delete", ResourceKind: "domain_binding", ResourceID: input.DomainID, CorrelationID: input.CorrelationID})
		if err != nil {
			return err
		}
		current, err := q.GetTunnelDomainV1(ctx, dbsqlc.GetTunnelDomainV1Params{DomainID: input.DomainID, AccountID: input.AccountID, TunnelID: input.TunnelID})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrDomainNotFound
		}
		if err != nil {
			return translate(err)
		}
		if replayed {
			result = ResourceMutationRecord{Domain: current, Operation: op, Replayed: true, Changed: op.Outcome == "changed"}
			return nil
		}
		if current.DeletedAt.Valid {
			op, err = completeResourceOperation(ctx, q, op.ID, current.ID, "ready", "unchanged", input.Now)
			if err != nil {
				return err
			}
			result = ResourceMutationRecord{Domain: current, Operation: op}
			return nil
		}
		if current.Generation != input.ExpectedGeneration {
			return ErrGenerationConflict
		}
		current, err = q.DeleteTunnelDomainV1(ctx, dbsqlc.DeleteTunnelDomainV1Params{Now: sql.NullTime{Time: input.Now, Valid: true}, QuarantineUntil: sql.NullTime{Time: input.Now.Add(7 * 24 * time.Hour), Valid: true}, DomainID: input.DomainID, AccountID: input.AccountID, TunnelID: input.TunnelID, ExpectedGeneration: input.ExpectedGeneration})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrGenerationConflict
		}
		if err != nil {
			return translate(err)
		}
		if err := tunnelcert.RevokeDurableCertificatesTx(ctx, tx, input.AccountID, input.TunnelID, input.DomainID, input.Now); err != nil {
			return err
		}
		current, err = q.GetTunnelDomainV1(ctx, dbsqlc.GetTunnelDomainV1Params{DomainID: input.DomainID, AccountID: input.AccountID, TunnelID: input.TunnelID})
		if err != nil {
			return err
		}
		op, err = completeResourceOperation(ctx, q, op.ID, current.ID, "ready", "changed", input.Now)
		if err != nil {
			return err
		}
		if err := recordResourceChange(ctx, q, resourceAuditInput{AuditEventID: input.AuditEventID, ParentAuditEventID: input.ParentAuditEventID, AccountID: input.AccountID,
			ActorID: auditActorID(input.ActorID, input.AuditActorID), ActorType: input.ActorType, EventType: "domain.deleted", ParentEventType: "tunnel.domain_deleted",
			ResourceType: "domain_binding", ResourceID: current.ID, TunnelID: input.TunnelID, OperationID: op.ID, IdempotencyKey: input.IdempotencyKey,
			RequestID: input.RequestID, CorrelationID: input.CorrelationID, SourceMachineID: input.SourceMachineID, Now: input.Now,
			Metadata: map[string]any{"state": "quarantined"}, Message: "domain binding and certificates revoked; user DNS preserved"}); err != nil {
			return err
		}
		result = ResourceMutationRecord{Domain: current, Operation: op, Changed: true}
		return nil
	})
	return result, translate(err)
}

func (r *SQLRepository) BeginResourceDomainVerification(ctx context.Context, input DomainRecord) (ResourceMutationRecord, error) {
	if err := validateDomainRecord(input, true); err != nil {
		return ResourceMutationRecord{}, err
	}
	var result ResourceMutationRecord
	err := r.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if err := authorizeTunnelManagementTx(ctx, tx); err != nil {
			return err
		}
		q := tx.Queries()
		tunnel, err := q.GetTunnelForResourceV1(ctx, dbsqlc.GetTunnelForResourceV1Params{TunnelID: input.TunnelID, AccountID: input.AccountID})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return translate(err)
		}
		if tunnel.DesiredState == DesiredDeleted {
			return ErrTerminalState
		}
		op, replayed, err := beginResourceOperation(ctx, q, resourceOperationInput{ID: input.OperationID, AccountID: input.AccountID, IdempotencyKey: input.IdempotencyKey, RequestHash: input.RequestHash, OperationType: "domain.verify", ResourceKind: "domain_binding", ResourceID: input.DomainID, CorrelationID: input.CorrelationID})
		if err != nil {
			return err
		}
		current, err := q.GetTunnelDomainV1(ctx, dbsqlc.GetTunnelDomainV1Params{DomainID: input.DomainID, AccountID: input.AccountID, TunnelID: input.TunnelID})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrDomainNotFound
		}
		if err != nil {
			return translate(err)
		}
		if replayed {
			result = ResourceMutationRecord{Domain: current, Operation: op, Replayed: true, Changed: op.Outcome == "changed"}
			return nil
		}
		if current.DeletedAt.Valid {
			return ErrTerminalState
		}
		if current.Generation != input.ExpectedGeneration {
			return ErrGenerationConflict
		}
		targets, err := publicationdns.ResolveTargetsTx(ctx, tx, "tunnel", input.TunnelID, tunnel.StableEndpoint)
		if err != nil {
			return err
		}
		records, err := json.Marshal(publicationdns.Records(targets, current.Hostname, current.DnsProvider))
		if err != nil {
			return err
		}
		current, err = q.BeginTunnelDomainVerificationV1(ctx, dbsqlc.BeginTunnelDomainVerificationV1Params{Now: input.Now, DomainID: input.DomainID, AccountID: input.AccountID, TunnelID: input.TunnelID, ExpectedGeneration: input.ExpectedGeneration})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrGenerationConflict
		}
		if err != nil {
			return translate(err)
		}
		if err = tx.QueryRow(ctx, `UPDATE paperboat.tunnel_domains SET ownership_state=CASE WHEN expected_records<>$3::jsonb THEN 'pending' ELSE ownership_state END,dns_target=$2,expected_records=$3::jsonb,observed_records='[]'::jsonb,dns_ttl_seconds=NULL,last_verified_at=NULL WHERE id=$1 AND generation=$4 RETURNING ownership_state`, current.ID, targets[0], records, current.Generation).Scan(&current.OwnershipState); err != nil {
			return err
		}
		current.DnsTarget = targets[0]
		current.ExpectedRecords = records
		current.ObservedRecords = []byte(`[]`)
		current.DnsTtlSeconds = sql.NullInt32{}
		current.LastVerifiedAt = sql.NullTime{}
		// Verification is intentionally not marked successful here. TRK-18 owns
		// external DNS observation and will perform the proof-bearing transition.
		op, err = advanceResourceOperation(ctx, q, op.ID, current.ID, "waiting_for_dns", 40, input.Now)
		if err != nil {
			return err
		}
		if err := recordResourceChange(ctx, q, resourceAuditInput{AuditEventID: input.AuditEventID, ParentAuditEventID: input.ParentAuditEventID, AccountID: input.AccountID,
			ActorID: auditActorID(input.ActorID, input.AuditActorID), ActorType: input.ActorType, EventType: "domain.verification_requested", ParentEventType: "tunnel.domain_verification_requested",
			ResourceType: "domain_binding", ResourceID: current.ID, TunnelID: input.TunnelID, OperationID: op.ID, IdempotencyKey: input.IdempotencyKey,
			RequestID: input.RequestID, CorrelationID: input.CorrelationID, SourceMachineID: input.SourceMachineID, Now: input.Now,
			Metadata: map[string]any{"state": "waiting_dns"}, Message: "DNS verification requested; no external proof observed yet"}); err != nil {
			return err
		}
		result = ResourceMutationRecord{Domain: current, Operation: op, Changed: true}
		return nil
	})
	return result, translate(err)
}

func validateDomainRecord(input DomainRecord, requireGeneration bool) error {
	if !requireGeneration && (strings.TrimSpace(input.RouteID) == "" || strings.TrimSpace(input.Hostname) == "") {
		return fmt.Errorf("%w: route and hostname are required for domain creation", ErrInvalidInput)
	}
	for name, value := range map[string]string{"operation_id": input.OperationID, "audit_event_id": input.AuditEventID, "account_id": input.AccountID, "tunnel_id": input.TunnelID, "domain_id": input.DomainID, "idempotency_key": input.IdempotencyKey, "actor_id": input.ActorID, "actor_type": input.ActorType, "correlation_id": input.CorrelationID} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%w: %s is required", ErrInvalidInput, name)
		}
	}
	if requireGeneration && input.ExpectedGeneration < 1 {
		return fmt.Errorf("%w: expected generation is required", ErrInvalidInput)
	}
	if input.RequestHash == ([sha256.Size]byte{}) {
		return fmt.Errorf("%w: request hash is required", ErrInvalidInput)
	}
	if !validActorType(input.ActorType) {
		return fmt.Errorf("%w: actor type is invalid", ErrInvalidInput)
	}
	return nil
}

func (r *SQLRepository) ListResourceTunnelLogs(ctx context.Context, accountID, tunnelID string, after int64, limit int) ([]dbsqlc.ListTunnelLogsV1Row, error) {
	if strings.TrimSpace(accountID) == "" || strings.TrimSpace(tunnelID) == "" || after < 0 || limit < 1 || limit > previewtunnelapi.MaximumPageLimit+1 {
		return nil, ErrInvalidInput
	}
	rows, err := r.db.Queries().ListTunnelLogsV1(ctx, dbsqlc.ListTunnelLogsV1Params{AccountID: accountID, TunnelID: nullableString(tunnelID), AfterSequence: sql.NullInt64{Int64: after, Valid: true}, RowLimit: int32(limit)})
	return rows, translate(err)
}

func (r *SQLRepository) ListResourcePreviewLogs(ctx context.Context, accountID, previewID string, after int64, limit int) ([]dbsqlc.TunnelLogEntry, error) {
	if strings.TrimSpace(accountID) == "" || strings.TrimSpace(previewID) == "" || after < 0 || limit < 1 || limit > previewtunnelapi.MaximumPageLimit+1 {
		return nil, ErrInvalidInput
	}
	rows, err := r.db.Queries().ListPreviewLogsV1(ctx, dbsqlc.ListPreviewLogsV1Params{AccountID: accountID, PreviewID: nullableString(previewID), AfterSequence: sql.NullInt64{Int64: after, Valid: true}, RowLimit: int32(limit)})
	return rows, translate(err)
}
