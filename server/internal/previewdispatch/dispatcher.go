// Package previewdispatch delivers one canonical preview lease projection to
// the selected online machine. It owns transport and credential minting, but
// never owns lease persistence or readiness state.
package previewdispatch

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pinksaucepasta/paperboat-server/internal/canonicaljson"
	"github.com/pinksaucepasta/paperboat-server/internal/db"
	"github.com/pinksaucepasta/paperboat-server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat-server/internal/mint"
	"github.com/pinksaucepasta/paperboat-server/internal/previewtunnelstore"
	"github.com/pinksaucepasta/paperboat-server/internal/previewv1"
	"github.com/pinksaucepasta/paperboat-server/internal/runtimeattachment"
)

const (
	DefaultTimeout = 25 * time.Second
	maximumTimeout = 30 * time.Second
	maxResponse    = 32 << 10
	maxRequest     = 32 << 10
)

var (
	ErrInvalidConfiguration = errors.New("preview dispatcher configuration is invalid")
	ErrMachineOffline       = errors.New("preview dispatch machine is offline")
	ErrRouteUnavailable     = errors.New("preview dispatch route is unavailable")
)

// DispatchError is safe for callers and logs. It deliberately does not carry
// response bodies, URLs containing credentials, or bearer tokens.
type DispatchError struct {
	Code      string
	Uncertain bool
	Retryable bool
	Cause     error
}

func (e *DispatchError) Error() string {
	if e == nil || e.Code == "" {
		return "preview dispatch failed"
	}
	return "preview dispatch " + e.Code
}

func (e *DispatchError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func (e *DispatchError) Is(target error) bool {
	other, ok := target.(*DispatchError)
	return ok && other != nil && e != nil && other.Code == e.Code && other.Uncertain == e.Uncertain
}

// UncertainOutcome lets the policy service classify transport failures without
// importing this transport package (which would create a package cycle).
func (e *DispatchError) UncertainOutcome() bool { return e != nil && e.Uncertain }

// MachineRoute is returned only for a currently eligible helper route. The
// database resolver always returns an HTTPS base URL. Tests and controlled
// local adapters may use an HTTP base URL, but never a URL with userinfo,
// query, fragment, or a non-root path.
type MachineRoute struct {
	Shared        bool
	EnvironmentID string
	BaseURL       string
}

type MachineRouteResolver interface {
	ResolvePreviewDispatchRoute(context.Context, string, string) (MachineRoute, error)
}

type Config struct {
	Resolver MachineRouteResolver
	Signer   *mint.Provider
	Issuer   string
	Client   *http.Client
	Timeout  time.Duration
	Now      func() time.Time
	NewJTI   func() (string, error)
}

type Dispatcher struct {
	resolver MachineRouteResolver
	signer   *mint.Provider
	issuer   string
	client   *http.Client
	timeout  time.Duration
	now      func() time.Time
	newJTI   func() (string, error)
}

func New(config Config) (*Dispatcher, error) {
	if config.Resolver == nil || config.Signer == nil || strings.TrimSpace(config.Issuer) == "" {
		return nil, ErrInvalidConfiguration
	}
	timeout := config.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	if timeout > maximumTimeout {
		return nil, ErrInvalidConfiguration
	}
	now := config.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	newJTI := config.NewJTI
	if newJTI == nil {
		newJTI = randomJTI
	}
	client := config.Client
	if client == nil {
		client = &http.Client{}
	}
	// A redirect would forward a one-shot credential to an untrusted host.
	// Clone the client so a caller's reusable client is not mutated.
	transportClient := *client
	transportClient.Timeout = timeout
	transportClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errors.New("preview dispatch redirect rejected")
	}
	return &Dispatcher{resolver: config.Resolver, signer: config.Signer, issuer: strings.TrimSpace(config.Issuer), client: &transportClient, timeout: timeout, now: now, newJTI: newJTI}, nil
}

// Dispatch validates the canonical projection, resolves the selected
// machine's active helper route, mints a short-lived operation-bound token,
// and makes one bounded HTTP request. It never changes lease state itself.
func (d *Dispatcher) Dispatch(ctx context.Context, request previewv1.DispatchRequest) (previewv1.DispatchOutcome, error) {
	if d == nil || ctx == nil {
		return previewv1.DispatchOutcome{}, ErrInvalidConfiguration
	}
	now := d.now().UTC()
	if err := request.Validate(now); err != nil {
		return previewv1.DispatchOutcome{}, err
	}
	if now.After(request.LeaseDeadline.UTC()) {
		return previewv1.DispatchOutcome{}, &DispatchError{Code: "lease_expired", Retryable: false, Cause: errLeaseExpired}
	}
	var route MachineRoute
	var err error
	if request.Lazy != nil {
		resolver, ok := d.resolver.(interface {
			ResolveLazyPreviewDispatchRoute(context.Context, previewv1.DispatchRequest) (MachineRoute, error)
		})
		if !ok {
			return previewv1.DispatchOutcome{}, ErrRouteUnavailable
		}
		route, err = resolver.ResolveLazyPreviewDispatchRoute(ctx, request)
	} else {
		route, err = d.resolver.ResolvePreviewDispatchRoute(ctx, request.AccountID, request.OwnerMachineID)
	}
	if err != nil {
		if pureRouteUnavailable(err) {
			return previewv1.DispatchOutcome{}, &DispatchError{Code: "machine_unavailable", Retryable: true, Cause: err}
		}
		return previewv1.DispatchOutcome{}, uncertainDispatchError("route_resolution_failed", err)
	}
	if route.Shared && request.AccessMode == "public" {
		return previewv1.DispatchOutcome{}, previewtunnelstore.ErrPreviewPublicationDenied
	}
	endpoint, err := dispatchEndpoint(route.BaseURL)
	if err != nil {
		return previewv1.DispatchOutcome{}, &DispatchError{Code: "route_unavailable", Retryable: true, Cause: err}
	}
	expiresAt := now.Add(2 * time.Minute)
	if request.LeaseDeadline.Before(expiresAt) {
		expiresAt = request.LeaseDeadline.UTC()
	}
	if !expiresAt.After(now) {
		return previewv1.DispatchOutcome{}, &DispatchError{Code: "lease_expired", Retryable: false, Cause: errLeaseExpired}
	}
	jti, err := d.newJTI()
	if err != nil || strings.TrimSpace(jti) == "" {
		return previewv1.DispatchOutcome{}, &DispatchError{Code: "credential_mint_failed", Uncertain: false, Retryable: true, Cause: err}
	}
	userDeadline := utcTime(request.UserDeadline)
	token, err := d.signer.SignCredential(mint.CredentialInput{
		Issuer: d.issuer, Audience: "paperboat-machine", Subject: request.ActorID, JTI: jti,
		IssuedAt: now, ExpiresAt: expiresAt, CredentialClass: "preview_launch", Scopes: []string{"preview:launch"},
		EnvironmentID: route.EnvironmentID, AccountID: request.AccountID, MachineID: request.OwnerMachineID,
		UserID: request.ActorID, ActorID: request.ActorID, OperationID: request.OperationID,
		PreviewID: request.PreviewID, OwnerSessionID: request.OwnerSessionID, OwnerSessionKind: request.OwnerSessionKind,
		IdempotencyKey: request.IdempotencyKey, RequestID: request.RequestID, CorrelationID: request.CorrelationID,
		TargetScheme: request.Target.Scheme, TargetAddress: request.Target.Address, AccessMode: request.AccessMode,
		Endpoint: request.Endpoint, LeaseDeadline: request.LeaseDeadline, UserDeadline: userDeadline,
		LeaseETag: request.LeaseETag, State: request.State, AllocationState: request.AllocationState,
		EdgeState: request.EdgeState, OriginState: request.OriginState, CreatedAt: request.CreatedAt,
		LastRenewedAt: request.LastRenewedAt, ExpectedGeneration: request.ExpectedGeneration, RequestHash: request.RequestHash,
	})
	if err != nil {
		return previewv1.DispatchOutcome{}, &DispatchError{Code: "credential_mint_failed", Retryable: false, Cause: err}
	}
	payload, err := json.Marshal(request)
	if err != nil || len(payload) > maxRequest {
		return previewv1.DispatchOutcome{}, &DispatchError{Code: "request_encoding_failed", Retryable: false, Cause: err}
	}
	requestCtx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return previewv1.DispatchOutcome{}, &DispatchError{Code: "request_encoding_failed", Retryable: false, Cause: err}
	}
	httpRequest.Header.Set("Authorization", "Bearer "+token)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")
	httpRequest.Header.Set("Cache-Control", "no-store")
	response, err := d.client.Do(httpRequest)
	if err != nil {
		return previewv1.DispatchOutcome{}, classifyTransportError(requestCtx, err)
	}
	outcome, decodeErr := decodeOutcome(response.Body, request, response.StatusCode)
	closeErr := response.Body.Close()
	if closeErr != nil {
		return previewv1.DispatchOutcome{}, uncertainDispatchError("remote_response_close_failed", errors.Join(decodeErr, closeErr))
	}
	if decodeErr != nil {
		return previewv1.DispatchOutcome{}, decodeErr
	}
	return outcome, nil
}

func pureRouteUnavailable(err error) (unavailable bool) {
	return allErrorLeavesMatch(err, ErrMachineOffline, ErrRouteUnavailable, sql.ErrNoRows)
}

func allErrorLeavesMatch(err error, allowed ...error) (matched bool) {
	defer func() {
		if recover() != nil {
			matched = false
		}
	}()
	if err == nil || len(allowed) == 0 {
		return false
	}
	nodes, found := 0, false
	var visit func(error, int) bool
	visit = func(current error, depth int) bool {
		if current == nil || depth >= 8 || nodes >= 32 {
			return false
		}
		typ := reflect.TypeOf(current)
		value := reflect.ValueOf(current)
		if typ == nil || (value.Kind() == reflect.Ptr || value.Kind() == reflect.Interface || value.Kind() == reflect.Map || value.Kind() == reflect.Slice || value.Kind() == reflect.Func) && value.IsNil() {
			return false
		}
		nodes++
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) == 0 {
				return false
			}
			for _, child := range children {
				if !visit(child, depth+1) {
					return false
				}
			}
			return true
		}
		if wrapped, ok := current.(interface{ Unwrap() error }); ok {
			return visit(wrapped.Unwrap(), depth+1)
		}
		if !typ.Comparable() {
			return false
		}
		for _, candidate := range allowed {
			if typ == reflect.TypeOf(candidate) && current == candidate {
				found = true
				return true
			}
		}
		return false
	}
	return visit(err, 0) && found
}

var errLeaseExpired = errors.New("preview lease deadline has passed")
var errRemoteResponseInvalid = errors.New("preview dispatch response is invalid")

func classifyTransportError(ctx context.Context, err error) error {
	if errors.Is(err, context.DeadlineExceeded) || ctx != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return uncertainDispatchError("timeout", err)
	}
	if allErrorLeavesMatch(err, context.Canceled) || err == nil && ctx != nil && errors.Is(ctx.Err(), context.Canceled) {
		return uncertainDispatchError("canceled", err)
	}
	return uncertainDispatchError("transport_failed", err)
}

func decodeOutcome(reader io.Reader, request previewv1.DispatchRequest, status int) (previewv1.DispatchOutcome, error) {
	raw, err := io.ReadAll(io.LimitReader(reader, maxResponse+1))
	if err != nil {
		return previewv1.DispatchOutcome{}, uncertainDispatchError("remote_response_read_failed", err)
	}
	if len(raw) > maxResponse {
		return previewv1.DispatchOutcome{}, uncertainDispatchError("remote_invalid_response", errRemoteResponseInvalid)
	}
	if err := canonicaljson.RejectDuplicateFields(raw); err != nil {
		return previewv1.DispatchOutcome{}, uncertainDispatchError("remote_invalid_response", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var outcome previewv1.DispatchOutcome
	if err := decoder.Decode(&outcome); err != nil {
		if status >= 500 || status == http.StatusRequestTimeout || status == http.StatusTooManyRequests {
			return previewv1.DispatchOutcome{}, uncertainDispatchError("remote_uncertain", err)
		}
		return previewv1.DispatchOutcome{}, uncertainDispatchError("remote_invalid_response", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			err = errRemoteResponseInvalid
		}
		return previewv1.DispatchOutcome{}, uncertainDispatchError("remote_invalid_response", err)
	}
	if outcome.Schema != previewv1.Schema || outcome.Kind != previewv1.PreviewDispatchKind || outcome.PreviewID != request.PreviewID || outcome.OperationID != request.OperationID || outcome.Generation < request.ExpectedGeneration {
		return previewv1.DispatchOutcome{}, uncertainDispatchError("remote_binding_mismatch", errRemoteResponseInvalid)
	}
	if outcome.State != "accepted" && outcome.State != "ready" && outcome.State != "failed" {
		return previewv1.DispatchOutcome{}, uncertainDispatchError("remote_invalid_response", errRemoteResponseInvalid)
	}
	if status < 200 || status >= 300 {
		if outcome.State == "failed" {
			return outcome, nil
		}
		if status >= 500 || status == http.StatusRequestTimeout || status == http.StatusTooManyRequests {
			return previewv1.DispatchOutcome{}, uncertainDispatchError("remote_uncertain", nil)
		}
		return previewv1.DispatchOutcome{}, &DispatchError{Code: "remote_rejected", Retryable: false}
	}
	return outcome, nil
}

func uncertainDispatchError(code string, cause error) *DispatchError {
	return &DispatchError{Code: code, Uncertain: true, Retryable: true, Cause: errors.Join(previewv1.ErrDispatchUncertain, cause)}
}

func dispatchEndpoint(base string) (string, error) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	parsed, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrRouteUnavailable, err)
	}
	if (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawPath != "" {
		return "", ErrRouteUnavailable
	}
	if parsed.Path != "" && parsed.Path != "/" {
		logical, ok := strings.CutPrefix(parsed.Path, "/v1/runtime/")
		if !ok || logical == "" || strings.ContainsAny(logical, "/:@?#") {
			return "", ErrRouteUnavailable
		}
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/v1/preview-launches"
	parsed.RawPath = ""
	return parsed.String(), nil
}

func utcTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	result := value.UTC()
	return &result
}

func randomJTI() (string, error) {
	value, err := uuid.NewRandom()
	if err != nil {
		return "", err
	}
	return "token_" + value.String(), nil
}

// DBMachineRouteResolver binds route eligibility to the selected user machine
// rather than merely looking up an environment route. This prevents a route
// for another online machine in the same environment from receiving a lease.
type DBMachineRouteResolver struct {
	DB *db.DB
}

func (r DBMachineRouteResolver) ResolvePreviewDispatchRoute(ctx context.Context, accountID, machineID string) (MachineRoute, error) {
	if r.DB == nil || strings.TrimSpace(accountID) == "" || strings.TrimSpace(machineID) == "" {
		return MachineRoute{}, ErrRouteUnavailable
	}
	var issuer, environment string
	var shared bool
	err := r.DB.SQL().QueryRowContext(ctx, `SELECT user_id,environment_id,m.user_id<>$2 OR m.owner_team_id IS NOT NULL FROM paperboat.user_machines m WHERE m.id=$1 AND paperboat.machine_capability_allowed($2,m.id,'preview_manage') AND m.state='online' AND m.seat_state='occupied' AND m.online AND m.revoked_at IS NULL AND m.deleted_at IS NULL AND m.disconnected_at IS NULL AND m.configured_capabilities @> ARRAY['preview_launch']::text[] AND m.observed_capabilities @> ARRAY['preview_launch']::text[]`, machineID, accountID).Scan(&issuer, &environment, &shared)
	if errors.Is(err, sql.ErrNoRows) {
		return MachineRoute{}, ErrMachineOffline
	}
	if err != nil {
		return MachineRoute{}, err
	}

	route, err := r.DB.Queries().GetActiveHelperRouteForMachine(ctx, dbsqlc.GetActiveHelperRouteForMachineParams{MachineID: machineID, AccountID: issuer})
	if errors.Is(err, sql.ErrNoRows) {
		return MachineRoute{}, ErrRouteUnavailable
	}
	if err != nil {
		return MachineRoute{}, err
	}
	ingressURL, err := runtimeattachment.IngressURL(ctx, r.DB, environment, route.PublicHost)
	if err != nil {
		return MachineRoute{}, errors.Join(ErrRouteUnavailable, err)
	}
	return MachineRoute{Shared: shared, EnvironmentID: environment, BaseURL: ingressURL}, nil
}

// ResolveLazyPreviewDispatchRoute accepts only a live activation binding. It
// cannot grant ordinary preview management to a private-access-only machine.
func (r DBMachineRouteResolver) ResolveLazyPreviewDispatchRoute(ctx context.Context, request previewv1.DispatchRequest) (MachineRoute, error) {
	if r.DB == nil || request.Lazy == nil || request.AccessMode == "public" {
		return MachineRoute{}, ErrRouteUnavailable
	}
	store, err := previewtunnelstore.New(r.DB)
	if err != nil {
		return MachineRoute{}, err
	}
	shared, err := store.VerifyLazyPreviewLeaseV1(ctx, previewtunnelstore.LazyLeaseAuthority{AccountID: request.AccountID, MachineID: request.OwnerMachineID, OwnerSessionID: request.OwnerSessionID, OwnerSessionKind: request.OwnerSessionKind, TargetScheme: request.Target.Scheme, TargetAddress: request.Target.Address, Endpoint: request.Endpoint, ActivationID: request.IdempotencyKey, PreviewID: request.PreviewID, PolicyID: request.Lazy.PolicyID, PolicyGeneration: request.Lazy.PolicyGeneration, InstallationGeneration: request.Lazy.InstallationGeneration})
	if err != nil {
		if err == previewtunnelstore.ErrOwnerNotFound {
			return MachineRoute{}, ErrMachineOffline
		}
		return MachineRoute{}, err
	}
	route, err := r.DB.Queries().GetActiveHelperRouteForMachine(ctx, dbsqlc.GetActiveHelperRouteForMachineParams{MachineID: request.OwnerMachineID, AccountID: request.AccountID})
	if err != nil {
		if err == pgx.ErrNoRows {
			return MachineRoute{}, ErrRouteUnavailable
		}
		return MachineRoute{}, err
	}
	ingressURL, err := runtimeattachment.IngressURL(ctx, r.DB, route.EnvironmentID, route.PublicHost)
	if err != nil {
		return MachineRoute{}, errors.Join(ErrRouteUnavailable, err)
	}
	return MachineRoute{Shared: shared, EnvironmentID: route.EnvironmentID, BaseURL: ingressURL}, nil
}
