package previewdomain

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db"
	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/previewtunnelapi"
)

type API interface {
	List(context.Context, previewtunnelapi.RequestContext, string, string, int) (Page, error)
	Get(context.Context, previewtunnelapi.RequestContext, string, string) (DomainView, error)
	Create(context.Context, previewtunnelapi.RequestContext, string, Request) (MutationResult, error)
	Delete(context.Context, previewtunnelapi.RequestContext, string, string, MutationInput) (MutationResult, error)
	ReadyAliases(context.Context, previewtunnelapi.RequestContext, string) ([]ReadyAlias, error)
}

type BatchCreator interface {
	CreateForPreviewTx(context.Context, *db.Tx, BatchCreateRequest) ([]dbsqlc.PreviewDomain, error)
}

type Service struct {
	repository PreviewDomainRepository
	cursors    *cursorCodec
	now        func() time.Time
	newID      func(prefix string) (string, error)
}

func NewService(repository PreviewDomainRepository, config Config) (*Service, error) {
	if repository == nil {
		return nil, fmt.Errorf("%w: repository is required", ErrInvalidInput)
	}
	cursors, err := newCursorCodec(config.CursorKey)
	if err != nil {
		return nil, err
	}
	now := config.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	newID := config.NewID
	if newID == nil {
		newID = defaultID
	}
	return &Service{repository: repository, cursors: cursors, now: now, newID: newID}, nil
}

func (s *Service) List(ctx context.Context, request previewtunnelapi.RequestContext, previewID, rawCursor string, limit int) (Page, error) {
	if err := authorize(request, "read"); err != nil {
		return Page{}, err
	}
	if limit < 1 || limit > previewtunnelapi.MaximumPageLimit {
		return Page{}, fmt.Errorf("%w: list limit is invalid", ErrInvalidInput)
	}
	ctx, request, scopeErr := s.scopeWorkspacePreview(ctx, request, previewID)
	if scopeErr != nil {
		return Page{}, scopeErr
	}
	lease, err := s.repository.Lease(ctx, request.Actor.AccountID, previewID)
	if err != nil {
		return Page{}, err
	}
	if err := requireActiveLease(lease, s.now()); err != nil {
		return Page{}, err
	}
	position, err := s.cursors.Decode(rawCursor, request.Actor.AccountID, previewID)
	if err != nil {
		return Page{}, err
	}
	rows, err := s.repository.List(ctx, request.Actor.AccountID, previewID, position, limit+1)
	if err != nil {
		return Page{}, err
	}
	page := Page{Items: make([]DomainView, 0, minInt(len(rows), limit))}
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	for _, row := range rows {
		if row.PreviewGeneration != lease.Generation {
			continue
		}
		page.Items = append(page.Items, DomainViewFromRow(row))
	}
	if hasMore && len(rows) > 0 {
		last := rows[len(rows)-1]
		page.NextCursor, err = s.cursors.Encode(request.Actor.AccountID, previewID, last.CreatedAt, last.ID)
		if err != nil {
			return Page{}, err
		}
	}
	return page, nil
}

func (s *Service) Get(ctx context.Context, request previewtunnelapi.RequestContext, previewID, domainID string) (DomainView, error) {
	if err := authorize(request, "read"); err != nil {
		return DomainView{}, err
	}
	ctx, request, scopeErr := s.scopeWorkspacePreview(ctx, request, previewID)
	if scopeErr != nil {
		return DomainView{}, scopeErr
	}
	lease, err := s.repository.Lease(ctx, request.Actor.AccountID, previewID)
	if err != nil {
		return DomainView{}, err
	}
	if err := requireActiveLease(lease, s.now()); err != nil {
		return DomainView{}, err
	}
	domain, err := s.repository.Get(ctx, request.Actor.AccountID, previewID, domainID)
	if err != nil {
		return DomainView{}, err
	}
	if domain.PreviewGeneration != lease.Generation {
		return DomainView{}, ErrGenerationConflict
	}
	return DomainViewFromRow(domain), nil
}

func (s *Service) Create(ctx context.Context, request previewtunnelapi.RequestContext, previewID string, input Request) (MutationResult, error) {
	if err := authorize(request, "write"); err != nil {
		return MutationResult{}, err
	}
	ctx, request, scopeErr := s.scopeWorkspacePreview(ctx, request, previewID)
	if scopeErr != nil {
		return MutationResult{}, scopeErr
	}
	lease, err := s.repository.Lease(ctx, request.Actor.AccountID, previewID)
	if err != nil {
		return MutationResult{}, err
	}
	now := s.now().UTC()
	if err := requireActiveLease(lease, now); err != nil {
		return MutationResult{}, err
	}
	if err := authorizeOwner(request, lease); err != nil {
		return MutationResult{}, err
	}
	hostname, matchType, err := NormalizeHostname(input.Hostname)
	if err != nil {
		return MutationResult{}, err
	}
	provider, err := ValidateProvider(input.Provider)
	if err != nil {
		return MutationResult{}, err
	}
	strategy, err := ValidateCertificateStrategy(input.CertificateStrategy, matchType)
	if err != nil {
		return MutationResult{}, err
	}
	if err := validateMutation(input.Mutation, false); err != nil {
		return MutationResult{}, err
	}
	operationID, err := s.newID("operation")
	if err != nil {
		return MutationResult{}, err
	}
	auditID, err := s.newID("event")
	if err != nil {
		return MutationResult{}, err
	}
	domainID, err := s.newID("domain")
	if err != nil {
		return MutationResult{}, err
	}
	challengeID, err := s.newID("challenge")
	if err != nil {
		return MutationResult{}, err
	}
	target, err := StableTargetHostname(lease.Endpoint)
	if err != nil {
		return MutationResult{}, err
	}
	recordType := DNSRecordType(hostname, provider)
	expected, err := json.Marshal([]DNSRecord{{Name: hostname, Type: recordType, Value: target, TTL: 300}})
	if err != nil {
		return MutationResult{}, err
	}
	result, err := s.repository.Create(ctx, CreateRecord{OperationID: operationID, AuditEventID: auditID, AccountID: request.Actor.AccountID, PreviewID: previewID, DomainID: domainID, PreviewGeneration: lease.Generation, Hostname: hostname, MatchType: matchType, ChallengeReference: "dns-challenge://" + challengeID, DNSTarget: target, DNSProvider: provider, ExpectedRecords: expected, CertificateStrategy: strategy, IdempotencyKey: input.Mutation.IdempotencyKey, RequestHash: input.Mutation.RequestHash, ActorID: request.Actor.ActorID, ActorType: actorType(request.Actor), RequestID: request.RequestID, CorrelationID: request.CorrelationID, SourceMachineID: request.Actor.MachineID, Now: now})
	if err != nil {
		return MutationResult{}, err
	}
	return mutationResult(result, request.RequestID), nil
}

func (s *Service) Delete(ctx context.Context, request previewtunnelapi.RequestContext, previewID, domainID string, input MutationInput) (MutationResult, error) {
	if err := authorize(request, "write"); err != nil {
		return MutationResult{}, err
	}
	now := s.now().UTC()
	ctx, request, scopeErr := s.scopeWorkspacePreview(ctx, request, previewID)
	if scopeErr != nil {
		return MutationResult{}, scopeErr
	}
	lease, err := s.repository.Lease(ctx, request.Actor.AccountID, previewID)
	if err != nil {
		return MutationResult{}, err
	}
	if err := requireActiveLease(lease, now); err != nil {
		return MutationResult{}, err
	}
	if err := authorizeOwner(request, lease); err != nil {
		return MutationResult{}, err
	}
	if err := validateMutation(input, true); err != nil {
		return MutationResult{}, err
	}
	operationID, err := s.newID("operation")
	if err != nil {
		return MutationResult{}, err
	}
	auditID, err := s.newID("event")
	if err != nil {
		return MutationResult{}, err
	}
	record := MutationRecord{OperationID: operationID, AuditEventID: auditID, AccountID: request.Actor.AccountID, PreviewID: previewID, DomainID: domainID, ExpectedGeneration: input.ExpectedGeneration, IdempotencyKey: input.IdempotencyKey, RequestHash: input.RequestHash, ActorID: request.Actor.ActorID, ActorType: actorType(request.Actor), RequestID: request.RequestID, CorrelationID: request.CorrelationID, SourceMachineID: request.Actor.MachineID, Now: now}
	result, err := s.repository.Delete(ctx, record)
	if err != nil {
		return MutationResult{}, err
	}
	return mutationResult(result, request.RequestID), nil
}

func (s *Service) ReadyAliases(ctx context.Context, request previewtunnelapi.RequestContext, previewID string) ([]ReadyAlias, error) {
	if err := authorize(request, "read"); err != nil {
		return nil, err
	}
	ctx, request, scopeErr := s.scopeWorkspacePreview(ctx, request, previewID)
	if scopeErr != nil {
		return nil, scopeErr
	}
	lease, err := s.repository.Lease(ctx, request.Actor.AccountID, previewID)
	if err != nil {
		return nil, err
	}
	if err := requireActiveLease(lease, s.now()); err != nil {
		return nil, err
	}
	rows, err := s.repository.ReadyAliases(ctx, request.Actor.AccountID, previewID, s.now().UTC())
	if err != nil {
		return nil, err
	}
	aliases := make([]ReadyAlias, 0, len(rows))
	for _, record := range rows {
		row := record.Domain
		if row.AccountID != request.Actor.AccountID || row.PreviewID != previewID || row.PreviewGeneration != lease.Generation || row.DeletedAt.Valid || row.OwnershipState != "verified" || row.ConflictState != "clear" || row.CertificateState != "ready" || record.CertificateReference == "" || record.CertificateGeneration < 1 {
			continue
		}
		var wildcardLabels *int
		if row.MatchType == "one_label_wildcard" {
			labels := 1
			wildcardLabels = &labels
		}
		aliases = append(aliases, ReadyAlias{DomainID: row.ID, PreviewID: row.PreviewID, PreviewGeneration: row.PreviewGeneration, Hostname: row.Hostname, MatchType: row.MatchType, WildcardLabels: wildcardLabels, Generation: row.Generation, CertificateReference: record.CertificateReference, CertificateGeneration: record.CertificateGeneration})
	}
	return aliases, nil
}

// CreateForPreviewTx is the service-level batch seam used by the preview
// lease creator. It requires a concrete SQL repository because the caller
// owns the transaction; no operation or commit is emitted here.
func (s *Service) CreateForPreviewTx(ctx context.Context, tx *db.Tx, input BatchCreateRequest) ([]DomainView, error) {
	creator, ok := s.repository.(BatchCreator)
	if !ok {
		return nil, fmt.Errorf("%w: repository does not support transactional preview creation", ErrInvalidInput)
	}
	if input.ActorID != "" && input.AccountID == "" {
		return nil, ErrInvalidInput
	}
	rows, err := creator.CreateForPreviewTx(ctx, tx, input)
	if err != nil {
		return nil, err
	}
	views := make([]DomainView, 0, len(rows))
	for _, row := range rows {
		views = append(views, DomainViewFromRow(row))
	}
	return views, nil
}

func authorize(request previewtunnelapi.RequestContext, action string) error {
	return previewtunnelapi.Authorize(request.Actor, previewtunnelapi.AccessRequest{AccountID: request.Actor.AccountID, Resource: "previews", Action: action})
}

func authorizeOwner(request previewtunnelapi.RequestContext, lease LeaseContext) error {
	if request.Actor.MachineID != "" && request.Actor.MachineID != lease.OwnerMachineID {
		return ErrOwnerDenied
	}
	if request.Actor.HostID != "" && request.Actor.HostID != lease.OwnerMachineID {
		return ErrOwnerDenied
	}
	return nil
}

func validateMutation(input MutationInput, generation bool) error {
	if strings.TrimSpace(input.IdempotencyKey) == "" || input.RequestHash == ([sha256.Size]byte{}) || (generation && input.ExpectedGeneration < 1) {
		return ErrInvalidInput
	}
	return nil
}

func mutationResult(result RepositoryMutation, requestID string) MutationResult {
	return MutationResult{Domain: DomainViewFromRow(result.Domain), Operation: operationView(result.Operation, requestID), Replayed: result.Replayed, Changed: result.Changed}
}

func operationView(row dbsqlc.Operation, requestID string) previewtunnelapi.Operation {
	var operationError *previewtunnelapi.APIError
	if row.ErrorCode.Valid {
		operationError = &previewtunnelapi.APIError{Schema: Schema, Kind: "error", Code: row.ErrorCode.String, Component: "preview_domain", Message: "The preview domain operation needs attention.", Outcome: "uncertain", Retryable: true, RequestID: requestID, CorrelationID: row.CorrelationID}
	}
	return previewtunnelapi.Operation{Schema: Schema, Kind: "operation", ID: row.ID, ResourceKind: row.ResourceKind, ResourceID: nullableStringValue(row.ResourceID), Phase: row.Phase, State: row.State, Progress: int(row.Progress), Retrying: row.Retrying, NextRetryAt: nullableTimeValue(row.NextRetryAt), Error: operationError, CorrelationID: row.CorrelationID, CreatedAt: row.CreatedAt.UTC(), UpdatedAt: row.UpdatedAt.UTC()}
}

func actorType(actor previewtunnelapi.Actor) string {
	if actor.Role == "system_worker" {
		return "system"
	}
	if actor.HostID != "" {
		return "host"
	}
	return "user"
}

func nullableStringValue(value sql.NullString) string {
	if value.Valid {
		return value.String
	}
	return ""
}

func nullableTimeValue(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}
	t := value.Time.UTC()
	return &t
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

type cursorCodec struct{ key []byte }

type cursorPayload struct {
	Version   int       `json:"version"`
	AccountID string    `json:"account_id"`
	PreviewID string    `json:"preview_id"`
	CreatedAt time.Time `json:"created_at"`
	ID        string    `json:"id"`
}

func newCursorCodec(key []byte) (*cursorCodec, error) {
	if len(key) < 32 {
		return nil, fmt.Errorf("preview domain cursor key must contain at least 32 bytes")
	}
	return &cursorCodec{key: append([]byte(nil), key...)}, nil
}

func (c *cursorCodec) Encode(accountID, previewID string, createdAt time.Time, id string) (string, error) {
	payload, err := json.Marshal(cursorPayload{Version: 1, AccountID: accountID, PreviewID: previewID, CreatedAt: createdAt.UTC(), ID: id})
	if err != nil || len(payload) > 768 {
		return "", previewtunnelapi.ErrInvalidCursor
	}
	signature := c.sign(payload)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func (c *cursorCodec) Decode(raw, accountID, previewID string) (*ListPosition, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 2 || len(raw) > 2048 {
		return nil, previewtunnelapi.ErrInvalidCursor
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(parts[0])
	if err != nil || len(payload) == 0 || len(payload) > 768 {
		return nil, previewtunnelapi.ErrInvalidCursor
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if err != nil || len(signature) != sha256.Size || !hmac.Equal(signature, c.sign(payload)) {
		return nil, previewtunnelapi.ErrInvalidCursor
	}
	var value cursorPayload
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil || value.Version != 1 || value.AccountID != accountID || value.PreviewID != previewID || value.ID == "" || value.CreatedAt.IsZero() {
		return nil, previewtunnelapi.ErrInvalidCursor
	}
	return &ListPosition{CreatedAt: value.CreatedAt.UTC(), ID: value.ID}, nil
}

func (c *cursorCodec) sign(payload []byte) []byte {
	mac := hmac.New(sha256.New, c.key)
	_, _ = mac.Write(payload)
	return mac.Sum(nil)
}
