package previewtunnelstore

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
	"github.com/pinksaucepasta/paperboat/server/internal/canonicaljson"
	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/teams"
	"github.com/pinksaucepasta/paperboat/server/internal/tunnelendpoint"
	"github.com/pinksaucepasta/paperboat/server/internal/workspace"
)

var (
	ErrHostnameModeConflict    = errors.New("hostname already uses TLS pass-through on port 443")
	ErrConflict                = errors.New("preview tunnel persistence conflict")
	ErrConstraint              = errors.New("preview tunnel persistence constraint violation")
	ErrGenerationConflict      = errors.New("preview tunnel generation conflict")
	ErrIdempotencyConflict     = errors.New("idempotency key reused with different input")
	ErrInvalidInput            = errors.New("invalid preview tunnel persistence input")
	ErrNotFound                = errors.New("preview tunnel resource not found")
	ErrOperationNotCancellable = errors.New("preview tunnel operation cannot be cancelled safely")
)

type Store struct {
	db             *db.DB
	now            func() time.Time
	previewDomains PreviewDomainBatchCreator
}

type PreviewDomainBatchCreator interface {
	CreateForPreviewTx(context.Context, *db.Tx, PreviewDomainBatchInput) error
}

type PreviewDomainCreateRequest struct {
	Hostname            string
	Provider            string
	CertificateStrategy string
}

type PreviewDomainBatchInput struct {
	AccountID         string
	PreviewID         string
	PreviewGeneration int64
	StableEndpoint    string
	Domains           []PreviewDomainCreateRequest
	ActorID           string
	ActorType         string
	RequestID         string
	CorrelationID     string
	Now               time.Time
}

func (s *Store) ConfigurePreviewDomains(creator PreviewDomainBatchCreator) error {
	if s == nil || creator == nil {
		return fmt.Errorf("%w: preview domain batch creator is required", ErrInvalidInput)
	}
	s.previewDomains = creator
	return nil
}

func (s *Store) GetOperation(ctx context.Context, accountID, operationID string) (dbsqlc.Operation, error) {
	if strings.TrimSpace(accountID) == "" || strings.TrimSpace(operationID) == "" {
		return dbsqlc.Operation{}, ErrInvalidInput
	}
	owner, scopeErr := resolveWorkspaceOperationAccount(ctx, s.db.Pool(), accountID, operationID)
	if scopeErr != nil {
		return dbsqlc.Operation{}, scopeErr
	}
	accountID = owner
	operation, err := s.db.Queries().GetPreviewTunnelOperation(ctx, dbsqlc.GetPreviewTunnelOperationParams{
		ID: operationID, AccountID: accountID,
	})
	if err == nil && (operation.OperationType == "route.delete" || operation.OperationType == "route.create" || operation.OperationType == "route.patch") && (operation.State == "pending" || operation.State == "running") {
		// Deletion completes only after the committed serving config excludes the
		// route and every old assignment has acknowledged withdrawal. Polling also
		// owns this projection when deleting the final route yields no ready event.
		result, refreshErr := s.db.Pool().Exec(ctx, `UPDATE operations o
 SET state=CASE WHEN o.operation_type='route.delete' THEN 'succeeded' ELSE 'failed' END,
 phase=CASE WHEN o.operation_type='route.delete' THEN 'ready' ELSE 'failed' END,progress=100,retrying=false,next_retry_at=NULL,
 error_code=CASE WHEN o.operation_type='route.delete' THEN NULL ELSE 'route_deleted' END,completed_at=$3,updated_at=$3
 FROM tunnel_routes r JOIN tunnels t ON t.id=r.tunnel_id
 WHERE o.id=$2 AND o.account_id=$1 AND o.resource_kind='route' AND o.resource_id=r.id
 AND o.operation_type IN ('route.delete','route.create','route.patch') AND o.state IN ('pending','running') AND t.account_id=$1
 AND r.deleted_at IS NOT NULL
 AND (o.operation_type<>'route.delete' OR (
 NOT EXISTS (SELECT 1 FROM endpoint_routes er JOIN endpoint_edge_assignments x ON x.route_id=er.id WHERE er.tunnel_route_id=r.id AND x.desired='active')
 AND (t.desired_state='deleted' OR (
 (t.summary_code='ready' OR NOT EXISTS (SELECT 1 FROM tunnel_routes remaining WHERE remaining.tunnel_id=t.id AND remaining.desired_state='active' AND remaining.deleted_at IS NULL))
 AND EXISTS (
 SELECT 1 FROM tunnel_config_generations g WHERE g.tunnel_id=t.id AND g.activation_state='active'
 AND g.created_at>=r.deleted_at AND NOT EXISTS (
 SELECT 1 FROM jsonb_array_elements(convert_from(g.snapshot,'UTF8')::jsonb->'routes') configured WHERE configured->>'id'=r.id)
 )))))`, accountID, operationID, s.now())
		if refreshErr != nil {
			return dbsqlc.Operation{}, translate(refreshErr)
		}
		if result.RowsAffected() == 1 {
			operation, err = s.db.Queries().GetPreviewTunnelOperation(ctx, dbsqlc.GetPreviewTunnelOperationParams{ID: operationID, AccountID: accountID})
		}
	}
	if err == nil && (operation.OperationType == "route.create" || operation.OperationType == "route.patch") && (operation.State == "pending" || operation.State == "running") {
		// Adding or changing a route is done when the edge serves its current generation, which is
		// also what the tunnel status reports as ready. Nothing else marks these operations
		// succeeded, so a --wait on `route add` never ended.
		result, refreshErr := s.db.Pool().Exec(ctx, `UPDATE operations o
 SET state='succeeded',phase='ready',progress=100,outcome='changed',retrying=false,next_retry_at=NULL,error_code=NULL,completed_at=$3,updated_at=$3
 FROM tunnel_routes r JOIN tunnels t ON t.id=r.tunnel_id
 WHERE o.id=$2 AND o.account_id=$1 AND o.resource_kind='route' AND o.resource_id=r.id
 AND o.operation_type IN ('route.create','route.patch') AND o.state IN ('pending','running') AND t.account_id=$1
 AND r.deleted_at IS NULL AND r.desired_state='active'
 AND EXISTS (SELECT 1 FROM endpoint_routes er JOIN endpoint_edge_assignments x ON x.route_id=er.id
   WHERE er.tunnel_route_id=r.id AND er.revision=r.generation AND x.desired='active' AND x.observed_ready)`, accountID, operationID, s.now())
		if refreshErr != nil {
			return dbsqlc.Operation{}, translate(refreshErr)
		}
		if result.RowsAffected() == 1 {
			operation, err = s.db.Queries().GetPreviewTunnelOperation(ctx, dbsqlc.GetPreviewTunnelOperationParams{ID: operationID, AccountID: accountID})
		}
	}
	return operation, translate(err)
}

// GetPreviewLeaseCreateOperationV1 returns the durable create operation for a
// lease. Readiness callers supply the server-issued operation ID as their
// idempotency key; resolving it from the lease prevents a valid machine
// identity from replaying a different operation against this preview.
func (s *Store) GetPreviewLeaseCreateOperationV1(ctx context.Context, accountID, previewID string) (dbsqlc.Operation, error) {
	if strings.TrimSpace(accountID) == "" || strings.TrimSpace(previewID) == "" {
		return dbsqlc.Operation{}, ErrInvalidInput
	}
	operation, err := s.db.Queries().GetPreviewLeaseCreateOperationV1(ctx, dbsqlc.GetPreviewLeaseCreateOperationV1Params{
		AccountID:  accountID,
		ResourceID: sql.NullString{String: previewID, Valid: true},
	})
	return operation, translate(err)
}

type CancelOperationInput struct {
	OperationID     string
	AccountID       string
	AuditEventID    string
	ActorID         string
	ActorType       string
	RequestID       string
	CorrelationID   string
	SourceMachineID string
}

type CancelOperationResult struct {
	Operation dbsqlc.Operation
	Replayed  bool
}

func (s *Store) CancelOperation(ctx context.Context, input CancelOperationInput) (CancelOperationResult, error) {
	for name, value := range map[string]string{
		"operation_id": input.OperationID, "account_id": input.AccountID,
		"audit_event_id": input.AuditEventID, "actor_id": input.ActorID,
		"actor_type": input.ActorType, "correlation_id": input.CorrelationID,
	} {
		if strings.TrimSpace(value) == "" {
			return CancelOperationResult{}, fmt.Errorf("%w: %s is required", ErrInvalidInput, name)
		}
	}
	if !validActorType(input.ActorType) {
		return CancelOperationResult{}, fmt.Errorf("%w: unsupported actor type", ErrInvalidInput)
	}
	var result CancelOperationResult
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		if _, selected := workspace.FromContext(ctx); selected {
			if err := teams.Lock(ctx, tx); err != nil {
				return err
			}
		}
		owner, err := resolveWorkspaceOperationAccount(ctx, tx, input.AccountID, input.OperationID)
		if err != nil {
			return err
		}
		input.AccountID = owner
		queries := tx.Queries()
		operation, err := queries.GetPreviewTunnelOperation(ctx, dbsqlc.GetPreviewTunnelOperationParams{
			ID: input.OperationID, AccountID: input.AccountID,
		})
		if err != nil {
			return translate(err)
		}
		switch operation.State {
		case "cancelled":
			result = CancelOperationResult{Operation: operation, Replayed: true}
			return nil
		case "succeeded", "failed":
			result = CancelOperationResult{Operation: operation, Replayed: true}
			return nil
		case "running", "uncertain":
			return ErrOperationNotCancellable
		case "pending":
		default:
			return ErrOperationNotCancellable
		}
		now := s.now()
		operation, err = queries.CancelPendingPreviewTunnelOperation(ctx, dbsqlc.CancelPendingPreviewTunnelOperationParams{
			Now: now, ID: input.OperationID, AccountID: input.AccountID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrOperationNotCancellable
		}
		if err != nil {
			return translate(err)
		}
		metadata, err := canonicalSafeJSON([]byte(fmt.Sprintf(`{"operation_type":%q,"previous_state":"pending"}`, operation.OperationType)))
		if err != nil {
			return err
		}
		_, err = queries.InsertPreviewTunnelAuditEvent(ctx, dbsqlc.InsertPreviewTunnelAuditEventParams{
			ID: input.AuditEventID, AccountID: nullString(input.AccountID), ActorID: nullString(input.ActorID),
			ActorUserID: actorUserID(input.ActorType, input.ActorID), ActorType: input.ActorType, EventType: "operation.cancelled",
			ChangeType: "cancel", Outcome: operation.Outcome, ResourceType: "operation", ResourceID: operation.ID,
			IdempotencyKey: nullString("operation.cancel:" + operation.ID), RequestID: nullString(input.RequestID),
			CorrelationID: nullString(input.CorrelationID), SourceMachineID: nullString(input.SourceMachineID),
			Metadata: metadata, CreatedAt: now,
		})
		if err != nil {
			return translate(err)
		}
		result.Operation = operation
		return nil
	})
	return result, err
}

type ListEventsInput struct {
	AccountID, ResourceType, ResourceID string
	AfterSequence                       int64
	Limit                               int32
}

func (s *Store) ListEvents(ctx context.Context, input ListEventsInput) ([]dbsqlc.AuditEvent, error) {
	if input.AccountID == "" || input.ResourceType == "" || input.ResourceID == "" || input.AfterSequence < 0 || input.Limit < 1 || input.Limit > 201 {
		return nil, ErrInvalidInput
	}
	owner, scopeErr := s.ResolveWorkspaceResourceAccount(ctx, input.AccountID, input.ResourceType, input.ResourceID)
	if scopeErr != nil {
		return nil, scopeErr
	}
	input.AccountID = owner
	events, err := s.db.Queries().ListPreviewTunnelEvents(ctx, dbsqlc.ListPreviewTunnelEventsParams{
		AccountID: sql.NullString{String: input.AccountID, Valid: true}, ResourceType: input.ResourceType,
		ResourceID: input.ResourceID, AfterSequence: sql.NullInt64{Int64: input.AfterSequence, Valid: true}, RowLimit: input.Limit,
	})
	return events, translate(err)
}

func New(database *db.DB) (*Store, error) {
	if database == nil {
		return nil, fmt.Errorf("%w: database is required", ErrInvalidInput)
	}
	return &Store{db: database, now: func() time.Time { return time.Now().UTC() }}, nil
}

type CreateTunnelInput struct {
	OperationID      string
	TunnelID         string
	AuditEventID     string
	AccountID        string
	IdempotencyKey   string
	RequestHash      [sha256.Size]byte
	Name             string
	AccessMode       string
	StableEndpointID string
	StableEndpoint   string
	HostID           string
	ActorID          string
	ActorType        string
	ExpiresAt        sql.NullTime
	CorrelationID    string
	RequestID        string
	SourceMachineID  string
}

type CreateTunnelResult struct {
	Tunnel    dbsqlc.Tunnel
	Operation dbsqlc.Operation
	Replayed  bool
}

func (s *Store) CreateTunnel(ctx context.Context, input CreateTunnelInput) (CreateTunnelResult, error) {
	if err := validateCreateTunnel(input); err != nil {
		return CreateTunnelResult{}, err
	}
	var result CreateTunnelResult
	err := s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		queries := tx.Queries()
		operation, err := queries.CreatePreviewTunnelOperation(ctx, dbsqlc.CreatePreviewTunnelOperationParams{
			ID: input.OperationID, AccountID: input.AccountID, IdempotencyKey: input.IdempotencyKey,
			RequestHash: input.RequestHash[:], OperationType: "tunnel.create", ResourceKind: "tunnel",
			Phase: "persisting", State: "running", Progress: 20, Outcome: "unchanged",
			CorrelationID: input.CorrelationID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			existing, getErr := queries.GetPreviewTunnelOperationByIdempotency(ctx, dbsqlc.GetPreviewTunnelOperationByIdempotencyParams{
				AccountID: input.AccountID, IdempotencyKey: input.IdempotencyKey,
			})
			if getErr != nil {
				return translate(getErr)
			}
			if existing.OperationType != "tunnel.create" || !bytes.Equal(existing.RequestHash, input.RequestHash[:]) {
				return ErrIdempotencyConflict
			}
			if !existing.ResourceID.Valid {
				return fmt.Errorf("%w: replayed operation has no resource", ErrConflict)
			}
			tunnel, getErr := queries.GetPreviewTunnel(ctx, dbsqlc.GetPreviewTunnelParams{ID: existing.ResourceID.String, AccountID: input.AccountID})
			if getErr != nil {
				return translate(getErr)
			}
			result = CreateTunnelResult{Tunnel: tunnel, Operation: existing, Replayed: true}
			return nil
		}
		if err != nil {
			return translate(err)
		}

		now := s.now()
		tunnel, err := queries.CreatePreviewTunnel(ctx, dbsqlc.CreatePreviewTunnelParams{
			ID: input.TunnelID, AccountID: input.AccountID, Name: input.Name,
			DesiredState: "active", AccessMode: input.AccessMode, StableEndpointID: input.StableEndpointID,
			StableEndpoint: input.StableEndpoint, CreatedByHostID: input.HostID,
			CreatedByActorID: input.ActorID, ExpiresAt: input.ExpiresAt, SummaryCode: "pending", Now: now,
		})
		if err != nil {
			return translate(err)
		}
		operation, err = queries.CompletePreviewTunnelOperation(ctx, dbsqlc.CompletePreviewTunnelOperationParams{
			ResourceID: sql.NullString{String: tunnel.ID, Valid: true}, Phase: "ready", State: "succeeded",
			Progress: 100, Outcome: "changed", ResultReference: sql.NullString{String: tunnel.ID, Valid: true},
			UpdatedAt: now, CompletedAt: sql.NullTime{Time: now, Valid: true}, ID: operation.ID,
		})
		if err != nil {
			return translate(err)
		}
		metadata, err := json.Marshal(map[string]any{"access_mode": tunnel.AccessMode, "generation": tunnel.Generation})
		if err != nil {
			return err
		}
		_, err = queries.InsertPreviewTunnelAuditEvent(ctx, dbsqlc.InsertPreviewTunnelAuditEventParams{
			ID: input.AuditEventID, AccountID: nullString(input.AccountID), ActorID: nullString(input.ActorID),
			ActorUserID: actorUserID(input.ActorType, input.ActorID), ActorType: input.ActorType, EventType: "tunnel.created",
			ChangeType: "create", Outcome: "changed", ResourceType: "tunnel", ResourceID: tunnel.ID,
			IdempotencyKey: nullString(input.IdempotencyKey), RequestID: nullString(input.RequestID),
			CorrelationID: nullString(input.CorrelationID), SourceMachineID: nullString(input.SourceMachineID),
			Metadata: metadata, CreatedAt: now,
		})
		if err != nil {
			return translate(err)
		}
		result = CreateTunnelResult{Tunnel: tunnel, Operation: operation}
		return nil
	})
	return result, err
}

type UpdateTunnelStateInput struct {
	TunnelID           string
	AccountID          string
	DesiredState       string
	ExpectedGeneration int64
}

func (s *Store) UpdateTunnelState(ctx context.Context, input UpdateTunnelStateInput) (dbsqlc.Tunnel, error) {
	if input.TunnelID == "" || input.AccountID == "" || input.ExpectedGeneration < 1 {
		return dbsqlc.Tunnel{}, ErrInvalidInput
	}
	switch input.DesiredState {
	case "active", "paused", "deleted":
	default:
		return dbsqlc.Tunnel{}, ErrInvalidInput
	}
	tunnel, err := s.db.Queries().UpdatePreviewTunnelDesiredState(ctx, dbsqlc.UpdatePreviewTunnelDesiredStateParams{
		DesiredState: input.DesiredState, Now: s.now(), ID: input.TunnelID,
		AccountID: input.AccountID, ExpectedGeneration: input.ExpectedGeneration,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		_, getErr := s.db.Queries().GetPreviewTunnel(ctx, dbsqlc.GetPreviewTunnelParams{ID: input.TunnelID, AccountID: input.AccountID})
		if errors.Is(getErr, pgx.ErrNoRows) {
			return dbsqlc.Tunnel{}, ErrNotFound
		}
		if getErr != nil {
			return dbsqlc.Tunnel{}, translate(getErr)
		}
		return dbsqlc.Tunnel{}, ErrGenerationConflict
	}
	return tunnel, translate(err)
}

func (s *Store) CreateRoute(ctx context.Context, params dbsqlc.CreatePreviewTunnelRouteParams) (dbsqlc.TunnelRoute, error) {
	value, err := s.db.Queries().CreatePreviewTunnelRoute(ctx, params)
	return value, translate(err)
}

func (s *Store) CreateDomain(ctx context.Context, params dbsqlc.CreatePreviewTunnelDomainParams) (dbsqlc.TunnelDomain, error) {
	canonical, err := canonicalSafeJSON(params.ObservedRecords)
	if err != nil {
		return dbsqlc.TunnelDomain{}, err
	}
	params.ObservedRecords = canonical
	value, err := s.db.Queries().CreatePreviewTunnelDomain(ctx, params)
	return value, translate(err)
}

func (s *Store) CreatePreviewLease(ctx context.Context, params dbsqlc.CreatePreviewLeaseParams) (dbsqlc.PreviewLease, error) {
	if !ValidPreviewOwnerSessionKind(params.OwnerSessionKind) {
		return dbsqlc.PreviewLease{}, ErrInvalidInput
	}
	value, err := s.db.Queries().CreatePreviewLease(ctx, params)
	return value, translate(err)
}

func (s *Store) ActivateConfigGeneration(ctx context.Context, params dbsqlc.CreatePreviewTunnelConfigGenerationParams) (dbsqlc.TunnelConfigGeneration, error) {
	canonical, err := canonicalSafeJSON(params.Snapshot)
	if err != nil {
		return dbsqlc.TunnelConfigGeneration{}, err
	}
	if len(params.ContentHash) != sha256.Size || sha256.Sum256(canonical) != [sha256.Size]byte(params.ContentHash) {
		return dbsqlc.TunnelConfigGeneration{}, fmt.Errorf("%w: snapshot hash mismatch", ErrInvalidInput)
	}
	params.Snapshot = canonical
	params.ActivationState = "pending"
	params.ActivatedAt = sql.NullTime{}
	var result dbsqlc.TunnelConfigGeneration
	err = s.db.InTx(ctx, func(ctx context.Context, tx *db.Tx) error {
		queries := tx.Queries()
		created, err := queries.CreatePreviewTunnelConfigGeneration(ctx, params)
		if err != nil {
			return translate(err)
		}
		if _, err := queries.SupersedePreviewTunnelConfigGeneration(ctx, params.TunnelID); err != nil {
			return translate(err)
		}
		result, err = queries.ActivatePreviewTunnelConfigGeneration(ctx, dbsqlc.ActivatePreviewTunnelConfigGenerationParams{
			ActivatedAt: sql.NullTime{Time: params.Now, Valid: true}, TunnelID: params.TunnelID, Generation: created.Generation,
		})
		return translate(err)
	})
	return result, err
}

func validateCreateTunnel(input CreateTunnelInput) error {
	for name, value := range map[string]string{
		"operation_id": input.OperationID, "tunnel_id": input.TunnelID, "audit_event_id": input.AuditEventID,
		"account_id": input.AccountID, "idempotency_key": input.IdempotencyKey, "name": input.Name,
		"stable_endpoint_id": input.StableEndpointID, "stable_endpoint": input.StableEndpoint,
		"host_id": input.HostID, "actor_id": input.ActorID, "actor_type": input.ActorType, "correlation_id": input.CorrelationID,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%w: %s is required", ErrInvalidInput, name)
		}
	}
	if input.RequestHash == ([sha256.Size]byte{}) {
		return fmt.Errorf("%w: request hash is required", ErrInvalidInput)
	}
	if err := tunnelendpoint.ValidateID(input.StableEndpointID); err != nil {
		return fmt.Errorf("%w: stable endpoint identity: %v", ErrInvalidInput, err)
	}
	if err := validateManagedEndpoint(input.StableEndpoint); err != nil {
		return err
	}
	if input.AccessMode != "public" && input.AccessMode != "private" && input.AccessMode != "team" {
		return fmt.Errorf("%w: unsupported access mode", ErrInvalidInput)
	}
	if !validActorType(input.ActorType) {
		return fmt.Errorf("%w: unsupported actor type", ErrInvalidInput)
	}
	return nil
}

func validateManagedEndpoint(endpoint string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.Port() != "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return fmt.Errorf("%w: stable endpoint must be an HTTPS host-only URL", ErrInvalidInput)
	}
	host := strings.ToLower(parsed.Hostname())
	if host != parsed.Hostname() {
		return fmt.Errorf("%w: stable endpoint host must be lowercase", ErrInvalidInput)
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return fmt.Errorf("%w: stable endpoint must include a configured base domain", ErrInvalidInput)
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("%w: stable endpoint host is invalid", ErrInvalidInput)
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return fmt.Errorf("%w: stable endpoint host is invalid", ErrInvalidInput)
			}
		}
	}
	return nil
}

func validateSafeJSON(raw []byte) error {
	_, err := canonicalSafeJSON(raw)
	return err
}

func canonicalSafeJSON(raw []byte) ([]byte, error) {
	if err := canonicaljson.RejectDuplicateFields(raw); err != nil {
		return nil, fmt.Errorf("%w: invalid or duplicate-field JSON", ErrInvalidInput)
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("%w: invalid JSON", ErrInvalidInput)
	}
	if forbiddenJSONKey(value) {
		return nil, fmt.Errorf("%w: reusable credential or payload field is forbidden", ErrInvalidInput)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("%w: canonicalize JSON", ErrInvalidInput)
	}
	return canonical, nil
}

func forbiddenJSONKey(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			normalized := strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.ToLower(key))
			switch normalized {
			case "accesstoken", "apikey", "authorization", "clientsecret", "cookie", "headers", "password", "privatekey", "refreshtoken", "requestbody", "requestheaders", "responsebody", "responseheaders", "secret", "sessiontoken", "setcookie", "token":
				return true
			}
			if forbiddenJSONKey(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if forbiddenJSONKey(child) {
				return true
			}
		}
	case string:
		upper := strings.ToUpper(strings.TrimSpace(typed))
		if strings.HasPrefix(upper, "BEARER ") || strings.Contains(upper, "BEGIN PRIVATE KEY") {
			return true
		}
		if parsed, err := url.Parse(typed); err == nil && parsed.IsAbs() {
			if parsed.User != nil {
				return true
			}
			for key := range parsed.Query() {
				normalized := strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.ToLower(key))
				switch normalized {
				case "accesstoken", "apikey", "authorization", "clientsecret", "password", "refreshtoken", "secret", "sessiontoken", "token":
					return true
				}
			}
		}
	}
	return false
}

func nullString(value string) sql.NullString {
	return sql.NullString{String: value, Valid: strings.TrimSpace(value) != ""}
}

func actorUserID(actorType, actorID string) sql.NullString {
	if actorType == "user" {
		return nullString(actorID)
	}
	return sql.NullString{}
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
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505":
			if pgErr.ConstraintName == "shared_443_hostname_mode_conflict" {
				return ErrHostnameModeConflict
			}
			return fmt.Errorf("%w: %s", ErrConflict, pgErr.ConstraintName)
		case "23503", "23514", "23502":
			return fmt.Errorf("%w: %s", ErrConstraint, pgErr.ConstraintName)
		}
	}
	return err
}
