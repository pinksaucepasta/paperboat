package previewv1

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/db/dbsqlc"
	"github.com/pinksaucepasta/paperboat/server/internal/previewdomain"
	"github.com/pinksaucepasta/paperboat/server/internal/previewtunnelapi"
	"github.com/pinksaucepasta/paperboat/server/internal/previewtunnelstore"
)

type previewLeaseFake struct {
	owners         map[string]bool
	shared         map[string]bool
	issuers        map[string]string
	leases         map[string]previewtunnelstore.PreviewLeaseRecord
	operations     map[string]dbsqlc.Operation
	createKeys     map[string]string
	createHashes   map[string]string
	renewKeys      map[string]previewtunnelstore.RenewPreviewLeaseV1Result
	readyCalls     int
	uncertainCalls int
	readyInputs    []previewtunnelstore.MarkPreviewLeaseReadyV1Input
	uncertainErr   error
	reconcile      previewtunnelstore.ReconcilePreviewLeasesV1Result
	createInput    []previewtunnelstore.CreatePreviewLeaseV1Input
	createErrors   []error
}

func newPreviewLeaseFake() *previewLeaseFake {
	return &previewLeaseFake{
		owners: make(map[string]bool), shared: make(map[string]bool), issuers: make(map[string]string), leases: make(map[string]previewtunnelstore.PreviewLeaseRecord),
		operations: make(map[string]dbsqlc.Operation), createKeys: make(map[string]string), createHashes: make(map[string]string),
		renewKeys: make(map[string]previewtunnelstore.RenewPreviewLeaseV1Result),
	}
}

func (f *previewLeaseFake) VerifyPreviewLeaseOwnerV1(_ context.Context, accountID, ownerMachineID, accessMode string) (bool, error) {
	if len(f.owners) > 0 && !f.owners[accountID+":"+ownerMachineID] {
		return false, previewtunnelstore.ErrOwnerNotFound
	}
	shared := f.shared[accountID+":"+ownerMachineID]
	if shared && accessMode == "public" {
		return false, previewtunnelstore.ErrPreviewPublicationDenied
	}
	return shared, nil
}

func (f *previewLeaseFake) GetPreviewLeaseV1(_ context.Context, accountID, previewID string) (previewtunnelstore.PreviewLeaseRecord, error) {
	lease, ok := f.leases[accountID+":"+previewID]
	if !ok {
		return previewtunnelstore.PreviewLeaseRecord{}, previewtunnelstore.ErrNotFound
	}
	return lease, nil
}

func (f *previewLeaseFake) GetPreviewLeaseCreateOperationV1(_ context.Context, accountID, previewID string) (dbsqlc.Operation, error) {
	for _, operation := range f.operations {
		if operation.AccountID == accountID && operation.OperationType == "preview.create" && operation.ResourceID.Valid && operation.ResourceID.String == previewID {
			return operation, nil
		}
	}
	return dbsqlc.Operation{}, previewtunnelstore.ErrNotFound
}

func (f *previewLeaseFake) ListPreviewLeasesV1(_ context.Context, input previewtunnelstore.ListPreviewLeasesV1Input) ([]previewtunnelstore.PreviewLeaseRecord, error) {
	items := make([]previewtunnelstore.PreviewLeaseRecord, 0)
	for key, lease := range f.leases {
		if strings.HasPrefix(key, input.AccountID+":") {
			items = append(items, lease)
		}
	}
	return items, nil
}

func (f *previewLeaseFake) CountPreviewLeasesV1(ctx context.Context, account string) (int, error) {
	items, err := f.ListPreviewLeasesV1(ctx, previewtunnelstore.ListPreviewLeasesV1Input{AccountID: account})
	return len(items), err
}

func (f *previewLeaseFake) CreatePreviewLeaseV1(_ context.Context, input previewtunnelstore.CreatePreviewLeaseV1Input) (previewtunnelstore.CreatePreviewLeaseV1Result, error) {
	f.createInput = append(f.createInput, input)
	if len(f.createErrors) > 0 {
		err := f.createErrors[0]
		f.createErrors = f.createErrors[1:]
		return previewtunnelstore.CreatePreviewLeaseV1Result{}, err
	}
	if existingID := f.createKeys[input.AccountID+":"+input.IdempotencyKey]; existingID != "" {
		if f.createHashes[input.AccountID+":"+input.IdempotencyKey] != string(input.RequestHash) {
			return previewtunnelstore.CreatePreviewLeaseV1Result{}, previewtunnelstore.ErrIdempotencyConflict
		}
		return previewtunnelstore.CreatePreviewLeaseV1Result{
			Lease: f.leases[input.AccountID+":"+existingID], Operation: f.operations[input.AccountID+":"+input.IdempotencyKey], Replayed: true,
		}, nil
	}
	lease := previewtunnelstore.PreviewLeaseRecord{PreviewLease: dbsqlc.PreviewLease{
		ID: input.LeaseID, Endpoint: input.Endpoint, AccountID: input.AccountID,
		ActorID: input.ActorID, OwnerMachineID: input.OwnerMachineID, OwnerSessionID: input.OwnerSessionID, OwnerSessionKind: input.OwnerSessionKind,
		TargetScheme: input.TargetScheme, TargetAddress: input.TargetAddress, AccessMode: input.AccessMode,
		LeaseDeadline: input.LeaseDeadline, UserDeadline: input.UserDeadline, AllocationState: "pending",
		EdgeState: "pending", OriginState: "unknown", TerminalState: "active", CreatedAt: input.Now,
		LastRenewedAt: input.Now, Generation: 1, OwnerLastSeenAt: input.Now,
	}}
	op := dbsqlc.Operation{ID: input.OperationID, AccountID: input.AccountID, IdempotencyKey: input.IdempotencyKey,
		RequestHash: input.RequestHash, OperationType: "preview.create", ResourceKind: "preview_lease",
		ResourceID: sql.NullString{String: input.LeaseID, Valid: true}, Phase: "connecting", State: "running", Progress: 60,
		Outcome: "changed", CorrelationID: input.CorrelationID, CreatedAt: input.Now, UpdatedAt: input.Now}
	f.leases[input.AccountID+":"+lease.ID] = lease
	f.operations[input.AccountID+":"+input.IdempotencyKey] = op
	f.createKeys[input.AccountID+":"+input.IdempotencyKey] = lease.ID
	f.createHashes[input.AccountID+":"+input.IdempotencyKey] = string(input.RequestHash)
	return previewtunnelstore.CreatePreviewLeaseV1Result{Lease: lease, Operation: op}, nil
}

func (f *previewLeaseFake) RenewPreviewLeaseV1(_ context.Context, input previewtunnelstore.RenewPreviewLeaseV1Input) (previewtunnelstore.RenewPreviewLeaseV1Result, error) {
	if result, ok := f.renewKeys[input.AccountID+":"+input.IdempotencyKey]; ok {
		result.Replayed = true
		return result, nil
	}
	key := input.AccountID + ":" + input.PreviewID
	lease, ok := f.leases[key]
	if !ok {
		return previewtunnelstore.RenewPreviewLeaseV1Result{}, previewtunnelstore.ErrNotFound
	}
	if lease.Generation != input.ExpectedGeneration {
		return previewtunnelstore.RenewPreviewLeaseV1Result{}, previewtunnelstore.ErrGenerationConflict
	}
	if lease.TerminalState != "active" {
		return previewtunnelstore.RenewPreviewLeaseV1Result{}, previewtunnelstore.ErrPreviewLeaseTerminal
	}
	lease.LeaseDeadline, lease.LastRenewedAt, lease.OwnerLastSeenAt = input.LeaseDeadline, input.Now, input.Now
	lease.Generation++
	f.leases[key] = lease
	op := dbsqlc.Operation{ID: input.OperationID, AccountID: input.AccountID, IdempotencyKey: input.IdempotencyKey,
		RequestHash: input.RequestHash, OperationType: "preview.renew", ResourceKind: "preview_lease",
		ResourceID: sql.NullString{String: input.PreviewID, Valid: true}, Phase: "ready", State: "succeeded", Progress: 100,
		Outcome: "changed", CorrelationID: input.CorrelationID, CreatedAt: input.Now, UpdatedAt: input.Now, CompletedAt: sql.NullTime{Time: input.Now, Valid: true}}
	result := previewtunnelstore.RenewPreviewLeaseV1Result{Lease: lease, Operation: op}
	f.renewKeys[input.AccountID+":"+input.IdempotencyKey] = result
	return result, nil
}

func (f *previewLeaseFake) StopPreviewLeaseV1(_ context.Context, input previewtunnelstore.StopPreviewLeaseV1Input) (previewtunnelstore.StopPreviewLeaseV1Result, error) {
	key := input.AccountID + ":" + input.PreviewID
	lease, ok := f.leases[key]
	if !ok {
		return previewtunnelstore.StopPreviewLeaseV1Result{}, previewtunnelstore.ErrNotFound
	}
	if lease.TerminalState != "active" {
		return previewtunnelstore.StopPreviewLeaseV1Result{Lease: lease, Replayed: true}, nil
	}
	if lease.Generation != input.ExpectedGeneration {
		return previewtunnelstore.StopPreviewLeaseV1Result{}, previewtunnelstore.ErrGenerationConflict
	}
	lease.AllocationState, lease.EdgeState, lease.TerminalState = "released", "released", "stopped"
	lease.StoppedAt = sql.NullTime{Time: input.Now, Valid: true}
	lease.Generation++
	f.leases[key] = lease
	return previewtunnelstore.StopPreviewLeaseV1Result{Lease: lease}, nil
}

func (f *previewLeaseFake) MarkPreviewLeaseReadyV1(_ context.Context, input previewtunnelstore.MarkPreviewLeaseReadyV1Input) (previewtunnelstore.PreviewLeaseRecord, error) {
	f.readyCalls++
	f.readyInputs = append(f.readyInputs, input)
	key := input.AccountID + ":" + input.PreviewID
	lease, ok := f.leases[key]
	if !ok {
		return previewtunnelstore.PreviewLeaseRecord{}, previewtunnelstore.ErrNotFound
	}
	if lease.Generation != input.ExpectedGeneration {
		return previewtunnelstore.PreviewLeaseRecord{}, previewtunnelstore.ErrGenerationConflict
	}
	lease.AllocationState, lease.EdgeState, lease.OriginState = input.AllocationState, input.EdgeState, input.OriginState
	if input.AllocationState == "failed" {
		lease.TerminalState = "failed"
		lease.StoppedAt = sql.NullTime{Time: input.Now, Valid: true}
	}
	if input.AllocationState == "ready" && input.EdgeState == "ready" && input.OriginState == "ready" && !lease.ReadyAt.Valid {
		lease.ReadyAt = sql.NullTime{Time: input.Now, Valid: true}
	}
	lease.Generation++
	f.leases[key] = lease
	return lease, nil
}

func (f *previewLeaseFake) MarkPreviewLeaseDispatchUncertainV1(_ context.Context, input previewtunnelstore.MarkPreviewLeaseDispatchUncertainV1Input) error {
	f.uncertainCalls++
	if f.uncertainErr != nil {
		return f.uncertainErr
	}
	for operationID, operation := range f.operations {
		if operation.AccountID == input.AccountID && operation.ResourceID.Valid && operation.ResourceID.String == input.PreviewID {
			operation.State, operation.ErrorCode, operation.Outcome = "uncertain", sql.NullString{String: input.ErrorCode, Valid: true}, "uncertain"
			operation.Retrying = true
			f.operations[operationID] = operation
			return nil
		}
	}
	return nil
}

func (f *previewLeaseFake) ReconcilePreviewLeasesV1(_ context.Context, _ previewtunnelstore.ReconcilePreviewLeasesV1Input) (previewtunnelstore.ReconcilePreviewLeasesV1Result, error) {
	return f.reconcile, nil
}

type incrementingReader struct{ next byte }

type previewDomainProjectionFake struct {
	rows []dbsqlc.PreviewDomain
}

func (f previewDomainProjectionFake) List(context.Context, string, string, *previewdomain.ListPosition, int) ([]dbsqlc.PreviewDomain, error) {
	return append([]dbsqlc.PreviewDomain(nil), f.rows...), nil
}

func (f previewDomainProjectionFake) ListProjections(_ context.Context, previews map[string]string) (map[string][]dbsqlc.PreviewDomain, error) {
	out := make(map[string][]dbsqlc.PreviewDomain, len(previews))
	for previewID := range previews {
		out[previewID] = append([]dbsqlc.PreviewDomain(nil), f.rows...)
	}
	return out, nil
}

type recordingDispatcher struct {
	calls     []DispatchRequest
	sessions  map[string]struct{}
	nextError error
	nextState string
}

func (d *recordingDispatcher) Dispatch(_ context.Context, request DispatchRequest) (DispatchOutcome, error) {
	d.calls = append(d.calls, request)
	if d.sessions == nil {
		d.sessions = make(map[string]struct{})
	}
	d.sessions[request.OperationID] = struct{}{}
	if d.nextError != nil {
		err := d.nextError
		d.nextError = nil
		return DispatchOutcome{}, err
	}
	if d.nextState != "" {
		state := d.nextState
		d.nextState = ""
		return DispatchOutcome{Schema: Schema, Kind: PreviewDispatchKind, PreviewID: request.PreviewID, OperationID: request.OperationID, State: state, Generation: request.ExpectedGeneration}, nil
	}
	return DispatchOutcome{Schema: Schema, Kind: PreviewDispatchKind, PreviewID: request.PreviewID, OperationID: request.OperationID, State: "accepted", Generation: request.ExpectedGeneration}, nil
}

type uncertainDispatchError struct{}

func (uncertainDispatchError) Error() string          { return "dispatch outcome is uncertain" }
func (uncertainDispatchError) UncertainOutcome() bool { return true }

type attachmentReadinessFake struct {
	err   error
	calls int
}

func (f *attachmentReadinessFake) RequirePreviewAttachmentReady(_ context.Context, accountID, previewID, operationID, ownerMachineID, ownerSessionID string) error {
	f.calls++
	if accountID == "" || previewID == "" || operationID == "" || ownerMachineID == "" || ownerSessionID == "" {
		return errors.New("incomplete attachment binding")
	}
	return f.err
}

func (r *incrementingReader) Read(dst []byte) (int, error) {
	for index := range dst {
		dst[index] = r.next
		r.next++
	}
	return len(dst), nil
}

func testPreviewLeaseService(t *testing.T, fake *previewLeaseFake, now time.Time) *Service {
	t.Helper()
	counter := 0
	service, err := NewService(fake, Config{
		EndpointDomain: "preview.example.test", CursorKey: []byte(strings.Repeat("k", 32)),
		LeaseDuration: 30 * time.Minute, MaxLease: 2 * time.Hour, OwnerGrace: 20 * time.Second,
		AttachmentReadiness: &attachmentReadinessFake{},
		Now:                 func() time.Time { return now }, Random: &incrementingReader{},
		NewID: func(prefix string) (string, error) { counter++; return fmt.Sprintf("%s_%03d", prefix, counter), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func browserRequest() previewtunnelapi.RequestContext {
	return previewtunnelapi.RequestContext{Actor: previewtunnelapi.Actor{AccountID: "acct_1", ActorID: "user_1", Role: "user"}, RequestID: "req_1", CorrelationID: "cor_1"}
}

func machineRequest(machine string) previewtunnelapi.RequestContext {
	request := browserRequest()
	request.Actor.MachineID = machine
	return request
}

func createInput(owner string) CreateRequest {
	return CreateRequest{OwnerMachineID: owner, OwnerSessionID: "session_1", OwnerSessionKind: "foreground", Target: Target{Scheme: "http", Address: "127.0.0.1:3000"}, IdempotencyKey: "create_1"}
}

func TestCreateDefaultsPublicAndAllocatesRandomEndpoint(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	fake := newPreviewLeaseFake()
	fake.owners["acct_1:machine_1"] = true
	service := testPreviewLeaseService(t, fake, now)
	result, err := service.Create(context.Background(), browserRequest(), createInput("machine_1"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Preview.AccessMode != "public" || result.Preview.Persistent {
		t.Fatalf("default preview policy = %#v", result.Preview)
	}
	if !regexp.MustCompile(`^https://[a-z]+-[a-z]+-[a-z]+-[0-9]{4}[.]preview[.]example[.]test$`).MatchString(result.Preview.Endpoint) {
		t.Fatalf("endpoint = %q", result.Preview.Endpoint)
	}
	if result.Preview.State != "allocating" || result.Operation.State != "running" {
		t.Fatalf("initial lifecycle = %#v operation=%#v", result.Preview, result.Operation)
	}
	if result.ETag == "" || strings.Contains(result.ETag, "machine") {
		t.Fatalf("etag leaked unexpected data: %q", result.ETag)
	}
	if len(fake.createInput) != 1 || fake.createInput[0].Endpoint == "" {
		t.Fatalf("create input missing endpoint: %#v", fake.createInput)
	}
}

func TestCreatePersistsNormalizedPreviewDomainsInLeaseTransaction(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	fake := newPreviewLeaseFake()
	fake.owners["acct_1:machine_1"] = true
	service := testPreviewLeaseService(t, fake, now)
	input := createInput("machine_1")
	input.Domains = []string{"*.Apps.Example.COM", "demo.example.com"}
	if _, err := service.Create(context.Background(), browserRequest(), input); err != nil {
		t.Fatal(err)
	}
	if len(fake.createInput) != 1 || len(fake.createInput[0].Domains) != 2 {
		t.Fatalf("domain create input = %+v", fake.createInput)
	}
	if got := []string{fake.createInput[0].Domains[0].Hostname, fake.createInput[0].Domains[1].Hostname}; !slices.Equal(got, []string{"*.apps.example.com", "demo.example.com"}) {
		t.Fatalf("normalized domains = %v", got)
	}
	duplicate := createInput("machine_1")
	duplicate.IdempotencyKey = "create_duplicate"
	duplicate.Domains = []string{"DEMO.example.com", "demo.example.com"}
	if _, err := service.Create(context.Background(), browserRequest(), duplicate); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("duplicate domain error = %v", err)
	}
	if len(fake.createInput) != 1 {
		t.Fatalf("duplicate domains reached persistence: %d", len(fake.createInput))
	}
}

func TestPreviewResourceProjectsReleasedDomainSummaryWithoutTunnelFields(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	fake := newPreviewLeaseFake()
	fake.leases["acct_1:prv_1"] = previewtunnelstore.PreviewLeaseRecord{PreviewLease: dbsqlc.PreviewLease{
		ID: "prv_1", AccountID: "acct_1", ActorID: "user_1", OwnerMachineID: "machine_1", OwnerSessionID: "owner_1", OwnerSessionKind: "foreground",
		TargetScheme: "http", TargetAddress: "127.0.0.1:3000", AccessMode: "public", Endpoint: "https://a.preview.example.test", LeaseDeadline: now, AllocationState: "released",
		EdgeState: "released", OriginState: "down", TerminalState: "stopped", Generation: 3, CreatedAt: now.Add(-time.Hour), LastRenewedAt: now,
	}}
	service, err := NewService(fake, Config{
		EndpointDomain: "preview.example.test", CursorKey: []byte(strings.Repeat("k", 32)), PreviewDomains: previewDomainProjectionFake{rows: []dbsqlc.PreviewDomain{{
			ID: "pdom_1", AccountID: "acct_1", PreviewID: "prv_1", PreviewGeneration: 2, Hostname: "*.apps.example.test",
			MatchType: "one_label_wildcard", OwnershipState: "expired", DnsTarget: "a.preview.example.test",
			ObservedRecords: []byte(`[]`), DnsProvider: "generic", ExpectedRecords: []byte(`[]`), DnsNextCheckAt: now,
			CertificateStrategy: "managed", CertificateState: "revoked", CaaState: "ready", ConflictState: "quarantined",
			Generation: 5, CreatedAt: now.Add(-time.Hour), UpdatedAt: now,
		}}}, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Get(context.Background(), browserRequest(), "prv_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Preview.Domains) != 1 || result.Preview.Domains[0].State != "quarantined" || result.Preview.Domains[0].WildcardLabels == nil || *result.Preview.Domains[0].WildcardLabels != 1 {
		t.Fatalf("domain summaries = %#v", result.Preview.Domains)
	}
	encoded, err := json.Marshal(result.Preview)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	domain := wire["domains"].([]any)[0].(map[string]any)
	for _, forbidden := range []string{"schema", "kind", "account_id", "tunnel_id", "route_id"} {
		if _, exists := domain[forbidden]; exists {
			t.Fatalf("preview domain summary exposed %s: %s", forbidden, encoded)
		}
	}
}

func TestCreateBindsSignedMachineAndAllowsBrowserMachineSelection(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	fake := newPreviewLeaseFake()
	fake.owners["acct_1:machine_a"] = true
	fake.owners["acct_1:machine_b"] = true
	service := testPreviewLeaseService(t, fake, now)
	if _, err := service.Create(context.Background(), machineRequest("machine_a"), createInput("machine_b")); !errors.Is(err, ErrOwnerDenied) {
		t.Fatalf("machine reassignment error = %v", err)
	}
	if _, err := service.Create(context.Background(), machineRequest("machine_a"), createInput("machine_a")); err != nil {
		t.Fatalf("matching machine create: %v", err)
	}
	if _, err := service.Create(context.Background(), browserRequest(), CreateRequest{OwnerMachineID: "machine_b", OwnerSessionID: "session_2", OwnerSessionKind: "foreground", Target: Target{Scheme: "https", Address: "localhost:8443"}, IdempotencyKey: "create_2", AccessMode: "private"}); err != nil {
		t.Fatalf("browser machine selection: %v", err)
	}
	if fake.createInput[1].AccessMode != "private" || fake.createInput[1].TargetScheme != "https" {
		t.Fatalf("private create input = %#v", fake.createInput[1])
	}
}

func TestCreateRejectsLiveCLIClientSessionAsPreviewOwnerMachine(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	fake := newPreviewLeaseFake()
	// The fake models the production owner query: only an eligible online
	// user_machines ID is present. A live CLI session ID is deliberately not a
	// preview owner, even when it belongs to the same account.
	fake.owners["acct_1:machine_1"] = true
	service := testPreviewLeaseService(t, fake, now)
	if _, err := service.Create(context.Background(), browserRequest(), createInput("cli_session_1")); !errors.Is(err, previewtunnelstore.ErrOwnerNotFound) {
		t.Fatalf("live CLI session owner error = %v, want owner not found", err)
	}
	if len(fake.createInput) != 0 {
		t.Fatalf("rejected CLI session was persisted: %d create calls", len(fake.createInput))
	}
}

func TestBrowserCannotRenewAnOwnerSessionLease(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	fake := newPreviewLeaseFake()
	fake.owners["acct_1:machine_1"] = true
	service := testPreviewLeaseService(t, fake, now)
	created, err := service.Create(context.Background(), browserRequest(), createInput("machine_1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Renew(context.Background(), browserRequest(), created.Preview.ID, MutationRequest{ExpectedGeneration: 1, OwnerSessionID: "session_1", IdempotencyKey: "renew_browser"}); !errors.Is(err, ErrOwnerDenied) {
		t.Fatalf("browser renewal error = %v, want ownership denial", err)
	}
	if got := fake.leases["acct_1:"+created.Preview.ID].Generation; got != 1 {
		t.Fatalf("browser renewal changed lease generation to %d", got)
	}
	if stopped, err := service.Stop(context.Background(), browserRequest(), created.Preview.ID, MutationRequest{ExpectedGeneration: 1, IdempotencyKey: "stop_browser"}); err != nil || stopped.Preview.State != "stopped" {
		t.Fatalf("same-account browser stop = %#v, %v", stopped, err)
	}
}

func TestCreateDeadlineIsMaximumAndRenewStopUseGeneration(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	fake := newPreviewLeaseFake()
	fake.owners["acct_1:machine_1"] = true
	service := testPreviewLeaseService(t, fake, now)
	input := createInput("machine_1")
	deadline := now.Add(45 * time.Minute)
	input.ExpiresAt = &deadline
	created, err := service.Create(context.Background(), browserRequest(), input)
	if err != nil {
		t.Fatal(err)
	}
	if created.Preview.UserDeadline == nil || !created.Preview.UserDeadline.Equal(deadline) || !created.Preview.LeaseDeadline.Equal(deadline) {
		t.Fatalf("deadline projection = %#v", created.Preview)
	}
	renewed, err := service.Renew(context.Background(), machineRequest("machine_1"), created.Preview.ID, MutationRequest{ExpectedGeneration: 1, OwnerSessionID: "session_1", IdempotencyKey: "renew_1"})
	if err != nil {
		t.Fatal(err)
	}
	if renewed.Preview.LeaseDeadline.After(deadline) || renewed.Preview.State != "allocating" {
		t.Fatalf("renewal exceeded deadline: %#v", renewed.Preview)
	}
	stopped, err := service.Stop(context.Background(), machineRequest("machine_1"), created.Preview.ID, MutationRequest{ExpectedGeneration: 2, IdempotencyKey: "stop_1"})
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Preview.State != "stopped" || stopped.Preview.EdgeState != "down" || stopped.Preview.AllocationState != "released" {
		t.Fatalf("stop projection = %#v", stopped.Preview)
	}
	replayed, err := service.Stop(context.Background(), machineRequest("machine_1"), created.Preview.ID, MutationRequest{ExpectedGeneration: 3, IdempotencyKey: "stop_2"})
	if err != nil || !replayed.Replayed {
		t.Fatalf("idempotent terminal stop = %#v, %v", replayed, err)
	}
}

func TestCreateIdempotencyReplaysSamePreview(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	fake := newPreviewLeaseFake()
	fake.owners["acct_1:machine_1"] = true
	service := testPreviewLeaseService(t, fake, now)
	input := createInput("machine_1")
	first, err := service.Create(context.Background(), browserRequest(), input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Create(context.Background(), browserRequest(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Replayed || first.Preview.ID != second.Preview.ID || first.Preview.Endpoint != second.Preview.Endpoint || len(fake.createInput) != 2 {
		t.Fatalf("replay = %#v create calls=%d", second, len(fake.createInput))
	}
}

func TestCreateDispatchesBrowserOnceAndReplayDoesNotDuplicateHostSession(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	fake := newPreviewLeaseFake()
	fake.owners["acct_1:machine_1"] = true
	dispatcher := &recordingDispatcher{}
	service := testPreviewLeaseService(t, fake, now)
	service.ConfigureDispatcher(dispatcher)
	input := createInput("machine_1")
	first, err := service.Create(context.Background(), browserRequest(), input)
	if err != nil {
		t.Fatal(err)
	}
	// Presentation can hide the managed URL while activation retains its
	// original endpoint identity for idempotent daemon dispatch.
	lease := fake.leases["acct_1:"+first.Preview.ID]
	hiddenEndpoint := ""
	lease.PublicEndpoint = &hiddenEndpoint
	lease.EdgeMode = previewtunnelstore.EdgeModeSelfhost
	fake.leases["acct_1:"+first.Preview.ID] = lease
	second, err := service.Create(context.Background(), browserRequest(), input)
	if err != nil {
		t.Fatal(err)
	}
	if second.Preview.Endpoint != "" || second.Preview.EndpointState != EndpointCustomDomainNotReady || first.Preview.EndpointState != EndpointReady {
		t.Fatal("API replay exposed managed URL despite selfhost projection")
	}
	if !second.Replayed || first.Preview.ID != second.Preview.ID || first.Operation.ID != second.Operation.ID {
		t.Fatalf("replay changed durable identity: first=%#v second=%#v", first, second)
	}
	if len(dispatcher.calls) != 2 || len(dispatcher.sessions) != 1 {
		t.Fatalf("dispatch calls=%d host sessions=%d, want two idempotent deliveries and one session", len(dispatcher.calls), len(dispatcher.sessions))
	}
	if dispatcher.calls[0].RequestHash != dispatcher.calls[1].RequestHash || dispatcher.calls[0].OperationID != dispatcher.calls[1].OperationID {
		t.Fatalf("replay changed dispatch binding: first=%#v second=%#v", dispatcher.calls[0], dispatcher.calls[1])
	}
}

func TestCreateDispatchesOwnerMachineRequestToStableHostRuntime(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	fake := newPreviewLeaseFake()
	fake.owners["acct_1:machine_1"] = true
	dispatcher := &recordingDispatcher{}
	service := testPreviewLeaseService(t, fake, now)
	service.ConfigureDispatcher(dispatcher)
	created, err := service.Create(context.Background(), machineRequest("machine_1"), createInput("machine_1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(dispatcher.calls) != 1 || len(dispatcher.sessions) != 1 {
		t.Fatalf("owner machine dispatch calls=%d host sessions=%d, want one stable-host delivery", len(dispatcher.calls), len(dispatcher.sessions))
	}
	if dispatcher.calls[0].PreviewID != created.Preview.ID || dispatcher.calls[0].OperationID != created.Operation.ID || dispatcher.calls[0].OwnerMachineID != "machine_1" {
		t.Fatalf("owner machine dispatch binding = %#v", dispatcher.calls[0])
	}
}

func TestCreateRetriesSameLeaseAfterUncertainDispatchAndRejectsHashMismatch(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	fake := newPreviewLeaseFake()
	fake.owners["acct_1:machine_1"] = true
	dispatcher := &recordingDispatcher{nextError: uncertainDispatchError{}}
	service := testPreviewLeaseService(t, fake, now)
	service.ConfigureDispatcher(dispatcher)
	input := createInput("machine_1")
	_, err := service.Create(context.Background(), browserRequest(), input)
	if err == nil || !dispatchErrorUncertain(err) {
		t.Fatalf("first dispatch error = %v, want uncertain outcome", err)
	}
	if fake.uncertainCalls != 1 || len(dispatcher.calls) != 1 {
		t.Fatalf("uncertain bookkeeping calls=%d dispatches=%d", fake.uncertainCalls, len(dispatcher.calls))
	}
	previewID, operationID := dispatcher.calls[0].PreviewID, dispatcher.calls[0].OperationID
	retry, err := service.Create(context.Background(), browserRequest(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !retry.Replayed || retry.Preview.ID != previewID || retry.Operation.ID != operationID || len(dispatcher.calls) != 2 {
		t.Fatalf("uncertain retry = %#v dispatches=%d", retry, len(dispatcher.calls))
	}
	input.Target.Address = "127.0.0.1:4000"
	if _, err := service.Create(context.Background(), browserRequest(), input); !errors.Is(err, previewtunnelstore.ErrIdempotencyConflict) {
		t.Fatalf("mismatched replay = %v, want idempotency conflict", err)
	}
}

func TestCreatePreservesDispatchAndUncertainMarkerFailures(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	fake := newPreviewLeaseFake()
	fake.owners["acct_1:machine_1"] = true
	dispatchCause := errors.New("remote connection reset")
	markerCause := errors.New("uncertain marker storage failed")
	fake.uncertainErr = markerCause
	dispatcher := &recordingDispatcher{nextError: errors.Join(uncertainDispatchError{}, dispatchCause)}
	service := testPreviewLeaseService(t, fake, now)
	service.ConfigureDispatcher(dispatcher)
	_, err := service.Create(context.Background(), browserRequest(), createInput("machine_1"))
	if err == nil || !dispatchErrorUncertain(err) || !errors.Is(err, dispatchCause) || !errors.Is(err, markerCause) {
		t.Fatalf("dispatch and uncertainty-marker causes were not both preserved: %v", err)
	}
	if fake.uncertainCalls != 1 || fake.readyCalls != 0 {
		t.Fatalf("uncertain marker calls=%d readiness writes=%d", fake.uncertainCalls, fake.readyCalls)
	}
}

func TestCreateKnownDispatchFailureUsesPersistedOriginDownState(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	fake := newPreviewLeaseFake()
	fake.owners["acct_1:machine_1"] = true
	dispatcher := &recordingDispatcher{nextState: "failed"}
	service := testPreviewLeaseService(t, fake, now)
	service.ConfigureDispatcher(dispatcher)

	if _, err := service.Create(context.Background(), browserRequest(), createInput("machine_1")); err == nil {
		t.Fatal("known dispatch failure returned nil")
	}
	if len(fake.readyInputs) != 1 {
		t.Fatalf("known dispatch failure readiness updates = %d, want one", len(fake.readyInputs))
	}
	if got := fake.readyInputs[0].OriginState; got != "down" {
		t.Fatalf("persisted origin state = %q, want internal down state", got)
	}
}

func TestCreateRetriesOnlyTypedEndpointConflict(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	fake := newPreviewLeaseFake()
	fake.owners["acct_1:machine_1"] = true
	fake.createErrors = []error{previewtunnelstore.ErrEndpointConflict}
	service := testPreviewLeaseService(t, fake, now)
	if _, err := service.Create(context.Background(), browserRequest(), createInput("machine_1")); err != nil {
		t.Fatalf("typed endpoint conflict should retry: %v", err)
	}
	if len(fake.createInput) != 2 || fake.createInput[0].Endpoint == fake.createInput[1].Endpoint {
		t.Fatalf("retry inputs = %#v", fake.createInput)
	}

	fake = newPreviewLeaseFake()
	fake.owners["acct_1:machine_1"] = true
	fake.createErrors = []error{fmt.Errorf("wrapped unrelated conflict: %w", previewtunnelstore.ErrConflict)}
	service = testPreviewLeaseService(t, fake, now)
	if _, err := service.Create(context.Background(), browserRequest(), createInput("machine_1")); !errors.Is(err, previewtunnelstore.ErrConflict) {
		t.Fatalf("unrelated conflict = %v", err)
	}
	if len(fake.createInput) != 1 {
		t.Fatalf("unrelated conflict retried, inputs = %d", len(fake.createInput))
	}
}

func TestCreateRejectsNonLoopbackAndNonCanonicalTargets(t *testing.T) {
	cases := []string{
		"localhost", "http://localhost:3000", "http://user@127.0.0.1:3000", "127.0.0.1:bad",
		"127.0.0.1:0", "127.0.0.1:65536", "192.168.1.10:3000", "127.0.0.1:3000/path", "127.0.0.1:3000?x=1",
	}
	for _, address := range cases {
		t.Run(address, func(t *testing.T) {
			if validLocalTarget(address) {
				t.Fatalf("accepted invalid target %q", address)
			}
		})
	}
	for _, address := range []string{"127.0.0.1:3000", "[::1]:8443", "localhost:3000"} {
		if !validLocalTarget(address) {
			t.Fatalf("rejected local target %q", address)
		}
	}
}

func TestCreateValidatesPreviewTargetSchemesAndPrivateTCP(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	fake := newPreviewLeaseFake()
	fake.owners["acct_1:machine_1"] = true
	service := testPreviewLeaseService(t, fake, now)
	valid := []struct {
		name, scheme, address, access, key string
	}{
		{"h2c", "h2c", "127.0.0.1:5000", "public", "create_h2c"},
		{"unix", "unix", "/run/paperboat.sock", "public", "create_unix"},
		{"private tcp", "tcp", "10.0.0.4:5432", "private", "create_tcp"},
	}
	for _, test := range valid {
		t.Run(test.name, func(t *testing.T) {
			input := createInput("machine_1")
			input.Target = Target{Scheme: test.scheme, Address: test.address}
			input.AccessMode, input.IdempotencyKey = test.access, test.key
			if _, err := service.Create(context.Background(), browserRequest(), input); err != nil {
				t.Fatalf("create %s: %v", test.name, err)
			}
		})
	}
	invalid := []struct {
		name, scheme, address, access string
	}{
		{"public tcp", "tcp", "10.0.0.4:5432", "public"},
		{"remote http", "http", "10.0.0.4:80", "public"},
		{"relative unix", "unix", "app.sock", "private"},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			input := createInput("machine_1")
			input.Target = Target{Scheme: test.scheme, Address: test.address}
			input.AccessMode, input.IdempotencyKey = test.access, "invalid_"+test.name
			if _, err := service.Create(context.Background(), browserRequest(), input); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("create %s error = %v", test.name, err)
			}
		})
	}
}

func TestObserveReadinessCompletesProjectionAndReconcileMapsTerminalStates(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	fake := newPreviewLeaseFake()
	fake.owners["acct_1:machine_1"] = true
	service := testPreviewLeaseService(t, fake, now)
	created, err := service.Create(context.Background(), browserRequest(), createInput("machine_1"))
	if err != nil {
		t.Fatal(err)
	}
	ready, err := service.ObserveReadiness(context.Background(), browserRequest(), created.Preview.ID, 1, "ready", "ready", "ready")
	if err != nil {
		t.Fatal(err)
	}
	if ready.Preview.State != "ready" || ready.Preview.OriginState != "ready" || fake.readyCalls != 1 {
		t.Fatalf("ready projection = %#v calls=%d", ready.Preview, fake.readyCalls)
	}
	fake.reconcile = previewtunnelstore.ReconcilePreviewLeasesV1Result{
		Expired:   []previewtunnelstore.PreviewLeaseRecord{{PreviewLease: dbsqlc.PreviewLease{ID: "prv_exp", AccountID: "acct_1", ActorID: "user_1", OwnerMachineID: "m", OwnerSessionID: "s", OwnerSessionKind: "foreground", TargetScheme: "http", TargetAddress: "127.0.0.1:3000", AccessMode: "public", Endpoint: "https://exp.preview.example.test", LeaseDeadline: now.Add(-time.Minute), AllocationState: "released", EdgeState: "released", OriginState: "unknown", TerminalState: "expired", CreatedAt: now.Add(-time.Hour), LastRenewedAt: now.Add(-time.Hour), Generation: 2, OwnerLastSeenAt: now.Add(-time.Hour)}}},
		OwnerLost: []previewtunnelstore.PreviewLeaseRecord{{PreviewLease: dbsqlc.PreviewLease{ID: "prv_lost", AccountID: "acct_1", ActorID: "user_1", OwnerMachineID: "m", OwnerSessionID: "s", OwnerSessionKind: "foreground", TargetScheme: "http", TargetAddress: "127.0.0.1:3000", AccessMode: "private", Endpoint: "https://lost.preview.example.test", LeaseDeadline: now.Add(time.Hour), AllocationState: "released", EdgeState: "released", OriginState: "down", TerminalState: "owner_lost", CreatedAt: now.Add(-time.Hour), LastRenewedAt: now.Add(-time.Hour), Generation: 3, OwnerLastSeenAt: now.Add(-time.Hour)}}},
	}
	reconciled, err := service.Reconcile(context.Background(), "worker_1", "cor_reconcile", "req_reconcile")
	if err != nil {
		t.Fatal(err)
	}
	if reconciled.Expired[0].State != "expired" || reconciled.Expired[0].EdgeState != "down" || reconciled.OwnerLost[0].State != "owner_disconnected" || reconciled.OwnerLost[0].OriginState != "unavailable" {
		t.Fatalf("reconciliation projection = %#v", reconciled)
	}
	// A terminal lease shows no endpoint: endpoint_state ended, endpoint empty (Lane G/M report: they said ready).
	for _, ended := range []Preview{reconciled.Expired[0], reconciled.OwnerLost[0]} {
		if ended.EndpointState != EndpointEnded || ended.Endpoint != "" {
			t.Fatalf("terminal lease %s: endpoint_state %q, endpoint %q; want ended and none", ended.ID, ended.EndpointState, ended.Endpoint)
		}
	}
}

func TestObserveMachineReadinessRequiresOwnerAndReplaysAfterUncertainResponse(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	fake := newPreviewLeaseFake()
	fake.owners["acct_1:machine_1"] = true
	service := testPreviewLeaseService(t, fake, now)
	created, err := service.Create(context.Background(), browserRequest(), createInput("machine_1"))
	if err != nil {
		t.Fatal(err)
	}
	blocked := &attachmentReadinessFake{err: errors.New("edge is not ready")}
	service.attachmentReadiness = blocked
	if _, err := service.ObserveMachineReadiness(context.Background(), machineRequest("machine_1"), created.Preview.ID, created.Operation.ID, "machine_1", "session_1", created.ETag, 1, "ready", "ready", "ready"); !errors.Is(err, ErrAttachmentNotReady) || blocked.calls != 1 || fake.readyCalls != 0 {
		t.Fatalf("attachment readiness gate error=%v calls=%d lease_mutations=%d", err, blocked.calls, fake.readyCalls)
	}
	service.attachmentReadiness = &attachmentReadinessFake{}
	ready, err := service.ObserveMachineReadiness(context.Background(), machineRequest("machine_1"), created.Preview.ID, created.Operation.ID, "machine_1", "session_1", created.ETag, 1, "ready", "ready", "ready")
	if err != nil {
		t.Fatal(err)
	}
	if ready.Preview.State != "ready" || ready.ETag == created.ETag || fake.readyCalls != 1 {
		t.Fatalf("first readiness = %#v calls=%d", ready, fake.readyCalls)
	}
	// The host may not know whether the first response reached it. Reusing the
	// same operation/body with the old strong ETag returns the already-ready
	// projection rather than incorrectly producing a generation conflict.
	replayed, err := service.ObserveMachineReadiness(context.Background(), machineRequest("machine_1"), created.Preview.ID, created.Operation.ID, "machine_1", "session_1", created.ETag, 1, "ready", "ready", "ready")
	if err != nil || replayed.ETag != ready.ETag || fake.readyCalls != 1 {
		t.Fatalf("readiness replay = %#v err=%v calls=%d", replayed, err, fake.readyCalls)
	}
	if _, err := service.ObserveMachineReadiness(context.Background(), machineRequest("machine_2"), created.Preview.ID, created.Operation.ID, "machine_1", "session_1", created.ETag, 1, "ready", "ready", "ready"); !errors.Is(err, ErrOwnerDenied) {
		t.Fatalf("different owner machine error = %v", err)
	}
	if _, err := service.ObserveMachineReadiness(context.Background(), machineRequest("machine_1"), created.Preview.ID, created.Operation.ID, "machine_1", "other_session", created.ETag, 1, "ready", "ready", "ready"); !errors.Is(err, ErrOwnerDenied) {
		t.Fatalf("different owner session error = %v", err)
	}
	if _, err := service.ObserveMachineReadiness(context.Background(), machineRequest("machine_1"), created.Preview.ID, created.Operation.ID, "machine_1", "session_1", created.ETag, 1, "ready", "ready", "unavailable"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("non-ready lifecycle error = %v", err)
	}
}

func TestNewServiceRejectsInvalidEndpointDomainAndCursorKey(t *testing.T) {
	fake := newPreviewLeaseFake()
	_, err := NewService(fake, Config{EndpointDomain: "https://bad.example", CursorKey: []byte(strings.Repeat("k", 32))})
	if err == nil {
		t.Fatal("accepted endpoint URL instead of host")
	}
	_, err = NewService(fake, Config{EndpointDomain: "preview.example", CursorKey: []byte("short")})
	if err == nil {
		t.Fatal("accepted short cursor key")
	}
}

func TestPreviewCursorIsBounded(t *testing.T) {
	codec, err := newCursorCodec([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codec.Encode(previewCursor{AccountID: "acct_1", ID: strings.Repeat("x", 2000), CreatedAt: time.Now().UTC()}); !errors.Is(err, previewtunnelapi.ErrInvalidCursor) {
		t.Fatalf("oversized cursor payload error = %v", err)
	}
	if _, err := codec.Decode(strings.Repeat("x", maxPreviewCursorEncoded+1), "acct_1"); !errors.Is(err, previewtunnelapi.ErrInvalidCursor) {
		t.Fatalf("oversized cursor error = %v", err)
	}
}

func TestPreviewCursorRejectsTunnelCursorAndUnknownFields(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	codec, err := newCursorCodec(key)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	// tunnelv1 signs the same account/time/id tuple with version=1 but no
	// resource kind. A preview cursor must not accept that other family.
	tunnelPayload, err := json.Marshal(struct {
		Version   int       `json:"version"`
		AccountID string    `json:"account_id"`
		CreatedAt time.Time `json:"created_at"`
		ID        string    `json:"id"`
	}{1, "acct_1", now, "prv_1"})
	if err != nil {
		t.Fatal(err)
	}
	tunnelMAC := hmac.New(sha256.New, key)
	_, _ = tunnelMAC.Write(tunnelPayload)
	tunnelCursor := base64.RawURLEncoding.EncodeToString(tunnelPayload) + "." + base64.RawURLEncoding.EncodeToString(tunnelMAC.Sum(nil))
	if _, err := codec.Decode(tunnelCursor, "acct_1"); !errors.Is(err, previewtunnelapi.ErrInvalidCursor) {
		t.Fatalf("accepted tunnel cursor: %v", err)
	}

	previewPayload, err := json.Marshal(struct {
		Version   int       `json:"version"`
		Kind      string    `json:"kind"`
		AccountID string    `json:"account_id"`
		CreatedAt time.Time `json:"created_at"`
		ID        string    `json:"id"`
		Extra     string    `json:"extra"`
	}{1, Kind, "acct_1", now, "prv_1", "reject"})
	if err != nil {
		t.Fatal(err)
	}
	previewMAC := hmac.New(sha256.New, key)
	_, _ = previewMAC.Write(previewPayload)
	unknownFieldCursor := base64.RawURLEncoding.EncodeToString(previewPayload) + "." + base64.RawURLEncoding.EncodeToString(previewMAC.Sum(nil))
	if _, err := codec.Decode(unknownFieldCursor, "acct_1"); !errors.Is(err, previewtunnelapi.ErrInvalidCursor) {
		t.Fatalf("accepted preview cursor with unknown field: %v", err)
	}
}

var _ io.Reader = (*incrementingReader)(nil)

func (f *previewLeaseFake) ResolvePreviewManagementAccountV1(_ context.Context, actor, preview string) (string, error) {
	for _, lease := range f.leases {
		if lease.ID == preview && (lease.AccountID == actor || (f.shared[actor+":"+lease.OwnerMachineID] && lease.AccessMode != "public")) {
			return lease.AccountID, nil
		}
	}
	return "", previewtunnelstore.ErrNotFound
}

func (f *previewLeaseFake) ResolvePreviewLeaseMachineAccountV1(_ context.Context, issuer, machine, preview string) (string, error) {

	for _, lease := range f.leases {
		if lease.ID == preview && lease.OwnerMachineID == machine && (lease.AccountID == issuer || f.issuers[machine] == issuer) {
			return lease.AccountID, nil
		}
	}
	return "", previewtunnelstore.ErrOwnerNotFound
}

func TestSharedPreviewKeepsRequesterOwnershipAndBoundsRenewal(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	fake := newPreviewLeaseFake()
	fake.owners["acct_1:machine_1"] = true
	fake.shared["acct_1:machine_1"] = true
	fake.issuers["machine_1"] = "enroller"
	service := testPreviewLeaseService(t, fake, now)
	input := createInput("machine_1")
	if _, err := service.Create(context.Background(), browserRequest(), input); !errors.Is(err, previewtunnelstore.ErrPreviewPublicationDenied) {
		t.Fatalf("default public shared publication: %v", err)
	}
	if len(fake.createInput) != 0 {
		t.Fatal("public denial occurred after persistence")
	}
	input.AccessMode = "private"
	created, err := service.Create(context.Background(), browserRequest(), input)
	if err != nil {
		t.Fatal(err)
	}
	if created.Preview.AccountID != "acct_1" || created.Preview.ActorID != "user_1" || created.Preview.LeaseDeadline.After(now.Add(5*time.Minute)) {
		t.Fatalf("shared owner/expiry changed: %+v", created.Preview)
	}
	proof := machineRequest("machine_1")
	proof.Actor.HostID = "machine_1"
	proof.Actor.AccountID = "enroller"
	proof.Actor.ActorID = "enroller"
	renewed, err := service.Renew(context.Background(), proof, created.Preview.ID, MutationRequest{ExpectedGeneration: 1, OwnerSessionID: "session_1", IdempotencyKey: "shared_renew"})
	if err != nil {
		t.Fatal(err)
	}
	if renewed.Preview.AccountID != "acct_1" || renewed.Preview.LeaseDeadline.After(now.Add(5*time.Minute)) {
		t.Fatal("renewal stole ownership or exceeded revocation bound")
	}
	fake.owners["acct_1:machine_1"] = false
	if _, err = service.Renew(context.Background(), proof, created.Preview.ID, MutationRequest{ExpectedGeneration: 2, OwnerSessionID: "session_1", IdempotencyKey: "shared_revoked"}); !errors.Is(err, previewtunnelstore.ErrOwnerNotFound) {
		t.Fatalf("revoked member renewal: %v", err)
	}
	other := proof
	other.Actor.MachineID = "machine_other"
	if _, err = service.Get(context.Background(), other, created.Preview.ID); !errors.Is(err, ErrOwnerDenied) {
		t.Fatalf("other machine accessed lease: %v", err)
	}
	if _, err = service.Get(context.Background(), proof, created.Preview.ID); err != nil {
		t.Fatalf("target cannot read revoked lease for cleanup: %v", err)
	}
}

func TestSharedManagerReadsAndStopsExistingPreviewWithoutRenewing(t *testing.T) {
	now := time.Now().UTC()
	fake := newPreviewLeaseFake()
	fake.owners["acct_1:machine_1"] = true
	service := testPreviewLeaseService(t, fake, now)
	input := createInput("machine_1")
	input.AccessMode = "private"
	created, err := service.Create(context.Background(), browserRequest(), input)
	if err != nil {
		t.Fatal(err)
	}
	manager := browserRequest()
	manager.Actor.AccountID, manager.Actor.ActorID, manager.Actor.MachineID = "member", "member", "cli_session"
	fake.shared["member:machine_1"] = true
	view, err := service.Get(context.Background(), manager, created.Preview.ID)
	if err != nil || view.Preview.AccountID != "acct_1" {
		t.Fatalf("manager read account=%q err=%v", view.Preview.AccountID, err)
	}
	mutation := MutationRequest{ExpectedGeneration: 1, OwnerSessionID: "session_1", IdempotencyKey: "manager_stop"}
	if _, err := service.Renew(context.Background(), manager, created.Preview.ID, mutation); !errors.Is(err, ErrOwnerDenied) {
		t.Fatalf("manager renewal: %v", err)
	}
	stopped, err := service.Stop(context.Background(), manager, created.Preview.ID, mutation)
	if err != nil || stopped.Preview.AccountID != "acct_1" {
		t.Fatalf("manager stop account=%q err=%v", stopped.Preview.AccountID, err)
	}
	fake.shared["member:machine_1"] = false
	if _, err := service.Stop(context.Background(), manager, created.Preview.ID, mutation); !errors.Is(err, ErrNotFound) {
		t.Fatalf("withdrawn manager replay: %v", err)
	}
}

func TestCreateOwnerSessionKindBindsIdentityAndDispatch(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	fake := newPreviewLeaseFake()
	fake.owners["acct_1:machine_1"] = true
	service := testPreviewLeaseService(t, fake, now)
	dispatcher := &recordingDispatcher{}
	service.ConfigureDispatcher(dispatcher)
	input := createInput("machine_1")
	input.OwnerSessionKind = "local_lease"
	result, err := service.Create(context.Background(), browserRequest(), input)
	if err != nil {
		t.Fatal(err)
	}
	if result.Preview.OwnerSessionKind != "local_lease" || len(dispatcher.calls) != 1 || dispatcher.calls[0].OwnerSessionKind != "local_lease" {
		t.Fatal("local lease kind lost before signed dispatch")
	}
	initial := hashCreateRequest(input)
	input.OwnerSessionKind = "foreground"
	if hashCreateRequest(input) == initial {
		t.Fatal("owner lifecycle kind not bound to create hash")
	}
	if _, err := service.Create(context.Background(), browserRequest(), input); !errors.Is(err, previewtunnelstore.ErrIdempotencyConflict) {
		t.Fatalf("kind change replay: %v", err)
	}
	for _, kind := range []string{"", "unknown", "lazy_runtime"} {
		input.OwnerSessionKind = kind
		input.IdempotencyKey = "invalid-kind-" + kind
		if _, err := service.Create(context.Background(), browserRequest(), input); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("invalid explicit lifecycle kind %q: %v", kind, err)
		}
	}
}

type previewCycleError struct{}

func (*previewCycleError) Error() string   { panic("private error text must not be evaluated") }
func (e *previewCycleError) Unwrap() error { return e }

type previewKnownDispatchFailure struct{ cause error }

func (*previewKnownDispatchFailure) Error() string          { return "preview dispatch failed" }
func (e *previewKnownDispatchFailure) Unwrap() error        { return e.cause }
func (*previewKnownDispatchFailure) UncertainOutcome() bool { return false }

func TestPreviewDispatchUncertaintyPreservesIndependentFailure(t *testing.T) {
	known := &previewKnownDispatchFailure{cause: syscall.EIO}
	var typedNil *previewKnownDispatchFailure
	for _, test := range []struct {
		name      string
		err       error
		uncertain bool
	}{
		{"known owner", fmt.Errorf("wrapped: %w", known), false},
		{"known with independent IO", errors.Join(known, syscall.EIO), true},
		{"unknown", syscall.EIO, true},
		{"cycle", &previewCycleError{}, true},
		{"typed nil", typedNil, true},
		{"known rejection", fmt.Errorf("wrapped: %w", ErrDispatchFailed), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := dispatchErrorUncertain(test.err); got != test.uncertain {
				t.Fatalf("uncertain = %v, want %v", got, test.uncertain)
			}
		})
	}

	fake := newPreviewLeaseFake()
	fake.owners["acct_1:machine_1"] = true
	service := testPreviewLeaseService(t, fake, time.Now().UTC())
	mixed := errors.Join(known, syscall.EIO)
	dispatcher := &recordingDispatcher{nextError: mixed}
	service.ConfigureDispatcher(dispatcher)
	input := createInput("machine_1")
	_, err := service.Create(context.Background(), browserRequest(), input)
	if !errors.Is(err, syscall.EIO) || fake.uncertainCalls != 1 || fake.readyCalls != 0 {
		t.Fatal("independent failure was consumed or marked known failed")
	}
	firstOperation := dispatcher.calls[0].OperationID
	if _, err := service.Create(context.Background(), browserRequest(), input); err != nil {
		t.Fatal(err)
	}
	if len(dispatcher.calls) != 2 || dispatcher.calls[1].OperationID != firstOperation || len(dispatcher.sessions) != 1 {
		t.Fatal("uncertain retry created a second operation")
	}
}

func TestPreviewEndpointMixedFailureDoesNotRetryAndRecovers(t *testing.T) {
	fake := newPreviewLeaseFake()
	fake.owners["acct_1:machine_1"] = true
	mixed := errors.Join(previewtunnelstore.ErrEndpointConflict, syscall.EIO)
	fake.createErrors = []error{mixed}
	service := testPreviewLeaseService(t, fake, time.Now().UTC())
	input := createInput("machine_1")
	if _, err := service.Create(context.Background(), browserRequest(), input); err != mixed || len(fake.createInput) != 1 {
		t.Fatal("mixed endpoint failure retried or lost its cause")
	}
	if _, err := service.Create(context.Background(), browserRequest(), input); err != nil {
		t.Fatal(err)
	}
	if len(fake.createInput) != 2 {
		t.Fatal("corrected request did not recover")
	}
	if isEndpointConflict(&previewCycleError{}) {
		t.Fatal("cyclic cause authorized retry")
	}
}

type previewOwnerFailureRepository struct {
	*previewLeaseFake
	err error
}

func (r *previewOwnerFailureRepository) ResolvePreviewLeaseMachineAccountV1(context.Context, string, string, string) (string, error) {
	return "", r.err
}

func TestPreviewOwnerDenialRetainsOriginalAndMixedFailure(t *testing.T) {
	fake := newPreviewLeaseFake()
	service := testPreviewLeaseService(t, fake, time.Now().UTC())
	original := fmt.Errorf("private lookup: %w", previewtunnelstore.ErrOwnerNotFound)
	repository := &previewOwnerFailureRepository{previewLeaseFake: fake, err: original}
	service.repository = repository
	_, err := service.machineLeaseRequest(context.Background(), machineRequest("machine_1"), "preview_1")
	if !errors.Is(err, ErrOwnerDenied) || !errors.Is(err, original) || err.Error() != "preview lease ownership denied" {
		t.Fatal("owner denial lost original cause or exposed lookup text")
	}
	repository.err = errors.Join(original, syscall.EIO)
	_, err = service.machineLeaseRequest(context.Background(), machineRequest("machine_1"), "preview_1")
	if err != repository.err || errors.Is(err, ErrOwnerDenied) {
		t.Fatal("independent failure became a proven denial")
	}
	repository.err = &previewCycleError{}
	_, err = service.machineLeaseRequest(context.Background(), machineRequest("machine_1"), "preview_1")
	if err != repository.err {
		t.Fatal("unresolved owner failure was replaced")
	}
}

func TestPreviewDomainProjectionOverflowIsInternalAndRecovers(t *testing.T) {
	service := testPreviewLeaseService(t, newPreviewLeaseFake(), time.Now().UTC())
	service.previewDomains = previewDomainProjectionFake{rows: make([]dbsqlc.PreviewDomain, previewdomain.MaxDomains+1)}
	if _, err := service.previewView(context.Background(), previewtunnelstore.PreviewLeaseRecord{}); err != ErrDomainProjection || errors.Is(err, ErrInvalidInput) {
		t.Fatal("stored projection overflow became client invalid input")
	}
	service.previewDomains = previewDomainProjectionFake{}
	if _, err := service.previewView(context.Background(), previewtunnelstore.PreviewLeaseRecord{}); err != nil {
		t.Fatal(err)
	}
}

func TestPreviewReadinessWaitsForManagedDNS(t *testing.T) {
	row := previewtunnelstore.PreviewLeaseRecord{PreviewLease: dbsqlc.PreviewLease{Endpoint: "https://preview.example.test", TerminalState: "active", AllocationState: "ready", EdgeState: "ready", OriginState: "ready"}, ManagedRoutePending: true}
	if got := previewView(row); got.State != "connecting" || got.EdgeState != "pending" || got.Endpoint != row.Endpoint {
		t.Fatal("unpublished preview reported ready")
	}
	row.ManagedRoutePending = false
	if got := previewView(row); got.State != "ready" || got.EdgeState != "ready" {
		t.Fatal("verified publication did not become ready")
	}
	row.ManagedRoutePending = true
	row.TerminalState = "stopped"
	if got := previewView(row); got.State != "stopped" {
		t.Fatal("DNS pending hid explicit stop")
	}
}

func TestPreviewReportsAStoppedLocalAppAsOriginUnavailable(t *testing.T) {
	row := previewtunnelstore.PreviewLeaseRecord{PreviewLease: dbsqlc.PreviewLease{Endpoint: "https://preview.example.test", TerminalState: "active", AllocationState: "ready", EdgeState: "ready", OriginState: "down"}}
	if got := previewView(row); got.State != "origin_unavailable" || got.OriginState != "unavailable" || got.EdgeState != "ready" {
		t.Fatalf("stopped app view = state %q origin %q edge %q", got.State, got.OriginState, got.EdgeState)
	}
	row.OriginState = "ready"
	if got := previewView(row); got.State != "ready" || got.OriginState != "ready" {
		t.Fatalf("recovered app view = state %q origin %q", got.State, got.OriginState)
	}
	// Before the edge is ready a missing origin is still just connecting.
	row.EdgeState, row.OriginState = "pending", "down"
	if got := previewView(row); got.State != "connecting" {
		t.Fatalf("pending edge with a down origin = %q, want connecting", got.State)
	}
	row.EdgeState, row.TerminalState = "ready", "stopped"
	if got := previewView(row); got.State != "stopped" {
		t.Fatal("a down origin hid an explicit stop")
	}
	if !validDispatchState("origin_unavailable", "ready", "ready", "unavailable") {
		t.Fatal("a lease with a stopped app cannot be dispatched to its host")
	}
}

func TestHostReportsItsLocalAppDownAndBack(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	fake := newPreviewLeaseFake()
	fake.owners["acct_1:machine_1"] = true
	service := testPreviewLeaseService(t, fake, now)
	service.attachmentReadiness = &attachmentReadinessFake{}
	created, err := service.Create(context.Background(), browserRequest(), createInput("machine_1"))
	if err != nil {
		t.Fatal(err)
	}
	observe := func(etag string, origin string) PreviewResult {
		t.Helper()
		result, err := service.ObserveMachineReadiness(context.Background(), machineRequest("machine_1"), created.Preview.ID, created.Operation.ID, "machine_1", "session_1", etag, generationFromETag(etag), "ready", "ready", origin)
		if err != nil {
			t.Fatalf("origin %s: %v", origin, err)
		}
		return result
	}
	ready := observe(created.ETag, "ready")
	down := observe(ready.ETag, "down")
	if down.Preview.State != "origin_unavailable" || down.Preview.OriginState != "unavailable" || down.Preview.EdgeState != "ready" {
		t.Fatalf("stopped app = state %q origin %q edge %q", down.Preview.State, down.Preview.OriginState, down.Preview.EdgeState)
	}
	back := observe(down.ETag, "ready")
	if back.Preview.State != "ready" || back.Preview.OriginState != "ready" {
		t.Fatalf("recovered app = state %q origin %q", back.Preview.State, back.Preview.OriginState)
	}
	for _, invalid := range []string{"unavailable", "degraded", "unknown", ""} {
		if _, err := service.ObserveMachineReadiness(context.Background(), machineRequest("machine_1"), created.Preview.ID, created.Operation.ID, "machine_1", "session_1", back.ETag, generationFromETag(back.ETag), "ready", "ready", invalid); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("origin %q accepted from the host: %v", invalid, err)
		}
	}
}
