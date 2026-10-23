package sync

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/supportreport"
	pb "github.com/pinksaucepasta/paperboat/server/internal/sync/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type testAuth struct {
	mu      sync.Mutex
	revoked bool
	scopes  []string
	err     error
}

func (a *testAuth) Authenticate(_ context.Context, token string) (auth.ClientPrincipal, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return auth.ClientPrincipal{}, a.err
	}
	if a.revoked || token != "valid" {
		return auth.ClientPrincipal{}, auth.ErrUnauthenticated
	}
	return auth.ClientPrincipal{SessionID: "session", User: auth.User{ID: "owner"}, Scopes: a.scopes}, nil
}

type testAuthority struct {
	mu          sync.Mutex
	removed     bool
	calls       int
	mutations   int
	snapshotErr error
	setTagsErr  error
}

type directSyncStream struct {
	ctx     context.Context
	request *pb.SyncRequest
	sent    chan struct{}
}

func (s *directSyncStream) Recv() (*pb.SyncRequest, error) { return s.request, nil }
func (s *directSyncStream) Send(*pb.SyncResponse) error {
	select {
	case s.sent <- struct{}{}:
	default:
	}
	return nil
}
func (s *directSyncStream) SetHeader(metadata.MD) error  { return nil }
func (s *directSyncStream) SendHeader(metadata.MD) error { return nil }
func (*directSyncStream) SetTrailer(metadata.MD)         {}
func (s *directSyncStream) Context() context.Context     { return s.ctx }
func (*directSyncStream) SendMsg(any) error              { return nil }
func (*directSyncStream) RecvMsg(any) error              { return nil }

func (a *testAuthority) Snapshot(_ context.Context, p auth.ClientPrincipal, source string) (*pb.SyncResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	if a.snapshotErr != nil {
		return nil, a.snapshotErr
	}
	if p.User.ID != "owner" || source != "machine" {
		return nil, status.Error(codes.PermissionDenied, "denied")
	}
	out := &pb.SyncResponse{}
	if !a.removed {
		out.Peers = []*pb.PeerUpdate{{PeerId: "peer", Approved: true}}
	}
	return out, nil
}
func (a *testAuthority) SetTags(_ context.Context, p auth.ClientPrincipal, id string, _ []string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if p.User.ID != "owner" || id != "machine" {
		return status.Error(codes.PermissionDenied, "denied")
	}
	if a.setTagsErr != nil {
		return a.setTagsErr
	}
	a.mutations++
	return nil
}
func testClient(t *testing.T, a *testAuth, authority *testAuthority, reporters ...*supportreport.Service) (pb.SyncServiceClient, *Server) {
	t.Helper()
	svc, err := NewService(a, authority)
	if err != nil {
		t.Fatal(err)
	}
	if len(reporters) > 1 {
		t.Fatal("test client accepts one support reporter")
	}
	if len(reporters) == 1 {
		svc.SetSupportReporter(reporters[0])
	}
	svc.interval = 10 * time.Millisecond
	listener := bufconn.Listen(1 << 20)
	g := grpc.NewServer()
	svc.Register(g)
	go g.Serve(listener)
	server := &Server{grpcServer: g, listener: listener}
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient("passthrough:///sync", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return pb.NewSyncServiceClient(conn), server
}

type syncReportStore struct {
	mu      sync.Mutex
	records []supportreport.Record
	saved   chan struct{}
}

func (s *syncReportStore) Save(_ context.Context, record supportreport.Record) error {
	s.mu.Lock()
	s.records = append(s.records, record)
	s.mu.Unlock()
	select {
	case s.saved <- struct{}{}:
	default:
	}
	return nil
}
func (*syncReportStore) Lookup(context.Context, string, string, string, bool) ([]supportreport.Record, error) {
	return nil, nil
}
func (*syncReportStore) Cleanup(context.Context) error { return nil }
func (s *syncReportStore) snapshot() []supportreport.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]supportreport.Record(nil), s.records...)
}

func startSyncReporter(t *testing.T) (*supportreport.Service, *syncReportStore, *bytes.Buffer, func()) {
	t.Helper()
	store := &syncReportStore{saved: make(chan struct{}, 8)}
	reporter, err := supportreport.New(store, supportreport.Config{})
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	reporter.SetLogger(slog.New(slog.NewJSONHandler(&logs, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- reporter.Run(ctx) }()
	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Errorf("support reporter stopped: %v", err)
			}
		})
	}
	t.Cleanup(stop)
	return reporter, store, &logs, stop
}

func TestSetTagsUnexpectedFailuresAreSafeAndCorrelated(t *testing.T) {
	tests := []struct {
		name       string
		authErr    error
		operation  error
		requestRef string
	}{
		{
			name:       "permission status joined with database failure",
			operation:  errors.Join(status.Error(codes.PermissionDenied, "PRIVATE_STATUS_DETAIL"), syscall.EIO),
			requestRef: supportreport.NewReference(),
		},
		{
			name:       "cancellation joined with database failure",
			operation:  errors.Join(context.Canceled, syscall.EIO),
			requestRef: "invalid-reference-is-replaced",
		},
		{
			name:    "authentication rejection joined with database failure",
			authErr: errors.Join(auth.ErrUnauthenticated, syscall.EIO),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reporter, store, logs, stopReporter := startSyncReporter(t)
			authority := &testAuthority{setTagsErr: test.operation}
			authenticator := &testAuth{scopes: []string{"machines:connect"}, err: test.authErr}
			client, _ := testClient(t, authenticator, authority, reporter)
			ctx := authorized(t.Context())
			if test.requestRef != "" {
				ctx = metadata.AppendToOutgoingContext(ctx, "support-reference", test.requestRef)
			}
			var trailer metadata.MD
			_, err := client.SetTags(ctx, &pb.SetTagsRequest{MachineId: "machine", Tags: []string{"dev"}}, grpc.Trailer(&trailer))
			if status.Code(err) != codes.Unavailable {
				t.Fatalf("operational failure was downgraded: code=%s error=%v", status.Code(err), err)
			}
			if strings.Contains(err.Error(), "PRIVATE_STATUS_DETAIL") || strings.Contains(err.Error(), "permission denied") || strings.Contains(err.Error(), "input/output error") {
				t.Fatalf("gRPC response exposed private cause text: %v", err)
			}
			if len(trailer.Get("support-reference")) != 1 {
				t.Fatalf("support reference trailer missing: %v", trailer)
			}
			reference := trailer.Get("support-reference")[0]
			if !supportreport.ValidReference(reference) || test.requestRef != "" && supportreport.ValidReference(test.requestRef) && reference != test.requestRef {
				t.Fatalf("unexpected support reference %q for request %q", reference, test.requestRef)
			}
			if !strings.Contains(err.Error(), reference) {
				t.Fatalf("safe response omitted support reference: %v", err)
			}
			select {
			case <-store.saved:
			case <-time.After(time.Second):
				t.Fatal("support fault was not persisted")
			}
			stopReporter()
			records := store.snapshot()
			if len(records) != 1 {
				t.Fatalf("expected one final-owner report, got %d: %+v", len(records), records)
			}
			record := records[0]
			if record.Reference != reference || record.CorrelationID != reference || record.Component != "paperboat-server" || record.Operation != "service_lifecycle" || record.Stage != "service_error" || record.Code != "control_sync_failed" || record.Cause != "system_error" || record.Errno != int(syscall.EIO) || record.Outcome != "failed" || record.Severity != "error" {
				t.Fatalf("support record lost bounded cause or request identity: %+v", record)
			}
			if test.authErr == nil && record.OwnerID != "owner" || test.authErr != nil && record.OwnerID != "" {
				t.Fatalf("support record has incorrect authenticated owner: %+v", record)
			}
			var event map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &event); err != nil {
				t.Fatalf("expected one structured event: %v; logs=%s", err, logs.String())
			}
			if event["code"] != "control_sync_failed" || event["cause"] != "system_error" || event["support_reference"] != reference || event["correlation_id"] != reference || strings.Contains(logs.String(), "PRIVATE_STATUS_DETAIL") {
				t.Fatalf("local log lost correlation or exposed the source error: %s", logs.String())
			}
		})
	}
}

func TestSetTagsPureRejectionsRemainQuiet(t *testing.T) {
	for _, test := range []struct {
		name  string
		cause error
		want  codes.Code
	}{
		{name: "permission denial", cause: status.Error(codes.PermissionDenied, "PRIVATE_STATUS_DETAIL"), want: codes.PermissionDenied},
		{name: "user cancellation", cause: context.Canceled, want: codes.Canceled},
		{name: "expired credentials", cause: auth.ErrUnauthenticated, want: codes.Unauthenticated},
	} {
		t.Run(test.name, func(t *testing.T) {
			reporter, store, logs, stopReporter := startSyncReporter(t)
			authenticator := &testAuth{scopes: []string{"machines:connect"}}
			authority := &testAuthority{setTagsErr: test.cause}
			if test.cause == auth.ErrUnauthenticated {
				authenticator.err = test.cause
				authority.setTagsErr = nil
			}
			client, _ := testClient(t, authenticator, authority, reporter)
			var trailer metadata.MD
			_, err := client.SetTags(authorized(t.Context()), &pb.SetTagsRequest{MachineId: "machine", Tags: []string{"dev"}}, grpc.Trailer(&trailer))
			if status.Code(err) != test.want || strings.Contains(err.Error(), "PRIVATE_STATUS_DETAIL") {
				t.Fatalf("pure rejection changed or exposed details: %v", err)
			}
			if len(trailer.Get("support-reference")) != 0 {
				t.Fatalf("expected rejection included an operational support trailer: %v", trailer)
			}
			stopReporter()
			if records := store.snapshot(); len(records) != 0 {
				t.Fatalf("expected rejection was captured as an exception: %+v", records)
			}
			if logs.Len() != 0 {
				t.Fatalf("expected rejection emitted an error log: %s", logs.String())
			}
		})
	}
}

func TestSyncSnapshotFailureReportsSafeCorrelatedCause(t *testing.T) {
	reporter, store, logs, stopReporter := startSyncReporter(t)
	reference := supportreport.NewReference()
	authority := &testAuthority{snapshotErr: errors.Join(status.Error(codes.PermissionDenied, "PRIVATE_SNAPSHOT_DETAIL"), syscall.EIO)}
	client, _ := testClient(t, &testAuth{scopes: []string{"machines:read"}}, authority, reporter)
	ctx := metadata.AppendToOutgoingContext(authorized(t.Context()), "support-reference", reference)
	stream, err := client.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&pb.SyncRequest{MachineId: "machine"}); err != nil {
		t.Fatal(err)
	}
	_, err = stream.Recv()
	if status.Code(err) != codes.Unavailable || strings.Contains(err.Error(), "PRIVATE_SNAPSHOT_DETAIL") {
		t.Fatalf("stream exposed or downgraded snapshot failure: %v", err)
	}
	if !strings.Contains(err.Error(), reference) || len(stream.Trailer().Get("support-reference")) != 1 || stream.Trailer().Get("support-reference")[0] != reference {
		t.Fatalf("stream response lost support reference: error=%v trailers=%v", err, stream.Trailer())
	}
	select {
	case <-store.saved:
	case <-time.After(time.Second):
		t.Fatal("snapshot failure was not persisted")
	}
	stopReporter()
	records := store.snapshot()
	if len(records) != 1 || records[0].Reference != reference || records[0].CorrelationID != reference || records[0].OwnerID != "owner" || records[0].Cause != "system_error" || records[0].Errno != int(syscall.EIO) {
		t.Fatalf("snapshot report lost its original cause or owner: %+v", records)
	}
	var event map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &event); err != nil {
		t.Fatalf("expected one structured snapshot failure log: %v; logs=%s", err, logs.String())
	}
	if event["support_reference"] != reference || event["correlation_id"] != reference || strings.Contains(logs.String(), "PRIVATE_SNAPSHOT_DETAIL") {
		t.Fatalf("snapshot failure log lost correlation or exposed provider details: %s", logs.String())
	}
}

func TestSyncInitialClientHalfCloseIsQuietAndReleasesStream(t *testing.T) {
	reporter, store, logs, stopReporter := startSyncReporter(t)
	client, _ := testClient(t, &testAuth{scopes: []string{"machines:read"}}, &testAuthority{}, reporter)
	stream, err := client.Sync(authorized(t.Context()))
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != io.EOF {
		t.Fatalf("initial client half-close did not finish normally: %v", err)
	}
	stopReporter()
	if records := store.snapshot(); len(records) != 0 {
		t.Fatalf("normal half-close created a fault: %+v", records)
	}
	if logs.Len() != 0 {
		t.Fatalf("normal half-close emitted an error log: %s", logs.String())
	}
}

func TestSyncCancellationPreservesIndependentCause(t *testing.T) {
	reporter, store, _, stopReporter := startSyncReporter(t)
	reference := supportreport.NewReference()
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer valid", "support-reference", reference))
	ctx, cancel := context.WithCancelCause(ctx)
	service, err := NewService(&testAuth{scopes: []string{"machines:read"}}, &testAuthority{})
	if err != nil {
		t.Fatal(err)
	}
	service.SetSupportReporter(reporter)
	service.interval = time.Millisecond
	stream := &directSyncStream{ctx: ctx, request: &pb.SyncRequest{MachineId: "machine"}, sent: make(chan struct{}, 1)}
	finished := make(chan error, 1)
	go func() { finished <- service.Sync(stream) }()
	select {
	case <-stream.sent:
	case <-time.After(time.Second):
		t.Fatal("sync stream did not reach its response loop")
	}
	cancel(syscall.EIO)
	err = <-finished
	if status.Code(err) != codes.Unavailable || !errors.Is(err, syscall.EIO) || !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "input/output error") {
		t.Fatalf("RPC did not retain private cancellation cause behind a safe status: %v", err)
	}
	select {
	case <-store.saved:
	case <-time.After(time.Second):
		t.Fatal("independent parent cancellation cause was discarded")
	}
	stopReporter()
	records := store.snapshot()
	if len(records) != 1 || records[0].Reference != reference || records[0].Cause != "system_error" || records[0].Errno != int(syscall.EIO) {
		t.Fatalf("cancellation cause did not reach one safe report: %+v", records)
	}
}

func TestSetTagsOperationalFailureRecoversOnNextRPC(t *testing.T) {
	reporter, store, _, stopReporter := startSyncReporter(t)
	reference := supportreport.NewReference()
	authority := &testAuthority{setTagsErr: syscall.EIO}
	client, _ := testClient(t, &testAuth{scopes: []string{"machines:connect"}}, authority, reporter)
	firstCtx := metadata.AppendToOutgoingContext(authorized(t.Context()), "support-reference", reference)
	_, firstErr := client.SetTags(firstCtx, &pb.SetTagsRequest{MachineId: "machine", Tags: []string{"dev"}})
	if status.Code(firstErr) != codes.Unavailable {
		t.Fatalf("first RPC did not expose a retryable failure: %v", firstErr)
	}
	authority.mu.Lock()
	authority.setTagsErr = nil
	authority.mu.Unlock()
	response, err := client.SetTags(authorized(t.Context()), &pb.SetTagsRequest{MachineId: "machine", Tags: []string{"dev"}})
	if err != nil || !response.GetSuccess() {
		t.Fatalf("fresh RPC did not recover after the operation became healthy: response=%v error=%v", response, err)
	}
	select {
	case <-store.saved:
	case <-time.After(time.Second):
		t.Fatal("failed RPC did not produce its support record")
	}
	stopReporter()
	records := store.snapshot()
	if len(records) != 1 || records[0].Reference != reference || records[0].Cause != "system_error" || records[0].Errno != int(syscall.EIO) {
		t.Fatalf("recovery emitted duplicate or lost the original failure: %+v", records)
	}
	authority.mu.Lock()
	mutations := authority.mutations
	authority.mu.Unlock()
	if mutations != 1 {
		t.Fatalf("fresh successful RPC was not applied exactly once: mutations=%d", mutations)
	}
}

func authorized(ctx context.Context) context.Context {
	return metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer valid"))
}
func TestAuthenticationPrecedesSnapshotAndMutation(t *testing.T) {
	a := &testAuth{scopes: []string{"machines:read", "machines:connect"}}
	authority := &testAuthority{}
	client, _ := testClient(t, a, authority)
	stream, err := client.Sync(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.Send(&pb.SyncRequest{MachineId: "machine", AuthToken: "valid"})
	_, err = stream.Recv()
	if status.Code(err) != codes.Unauthenticated {
		t.Fatal(err)
	}
	_, err = client.SetTags(t.Context(), &pb.SetTagsRequest{MachineId: "machine"})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatal(err)
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if authority.calls != 0 || authority.mutations != 0 {
		t.Fatal("unauthorized request reached authority")
	}
}
func TestSnapshotWithdrawalAndSessionRevocation(t *testing.T) {
	a := &testAuth{scopes: []string{"machines:read"}}
	authority := &testAuthority{}
	client, _ := testClient(t, a, authority)
	ctx, cancel := context.WithTimeout(authorized(t.Context()), time.Second)
	defer cancel()
	stream, err := client.Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = stream.Send(&pb.SyncRequest{MachineId: "machine"}); err != nil {
		t.Fatal(err)
	}
	first, err := stream.Recv()
	if err != nil || len(first.GetPeers()) != 1 {
		t.Fatalf("%v %v", first, err)
	}
	authority.mu.Lock()
	authority.removed = true
	authority.mu.Unlock()
	for {
		next, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if len(next.Peers) == 0 {
			if next.Revision <= first.Revision {
				t.Fatal("revision did not advance")
			}
			break
		}
	}
	a.mu.Lock()
	a.revoked = true
	a.mu.Unlock()
	for {
		_, err = stream.Recv()
		if err != nil {
			if status.Code(err) != codes.Unauthenticated {
				t.Fatal(err)
			}
			break
		}
	}
}
func TestOldStreamCloseDoesNotWithdrawReplacement(t *testing.T) {
	a := &testAuth{scopes: []string{"machines:read"}}
	authority := &testAuthority{}
	client, _ := testClient(t, a, authority)
	firstCtx, cancel := context.WithCancel(authorized(t.Context()))
	first, _ := client.Sync(firstCtx)
	_ = first.Send(&pb.SyncRequest{MachineId: "machine"})
	if _, err := first.Recv(); err != nil {
		t.Fatal(err)
	}
	second, _ := client.Sync(authorized(t.Context()))
	_ = second.Send(&pb.SyncRequest{MachineId: "machine"})
	if _, err := second.Recv(); err != nil {
		t.Fatal(err)
	}
	cancel()
	next, err := second.Recv()
	if err != nil || len(next.GetPeers()) != 1 {
		t.Fatalf("replacement lost: %v %v", next, err)
	}
}
func TestTagScopeAndExactMachineAuthorization(t *testing.T) {
	a := &testAuth{scopes: []string{"machines:read"}}
	authority := &testAuthority{}
	client, _ := testClient(t, a, authority)
	ctx := authorized(t.Context())
	_, err := client.SetTags(ctx, &pb.SetTagsRequest{MachineId: "machine", Tags: []string{"dev"}})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.scopes = append(a.scopes, "machines:connect")
	a.mu.Unlock()
	_, err = client.SetTags(ctx, &pb.SetTagsRequest{MachineId: "foreign"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatal(err)
	}
	_, err = client.SetTags(ctx, &pb.SetTagsRequest{MachineId: "machine", Tags: []string{"dev", "dev"}})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatal(err)
	}
	reply, err := client.SetTags(ctx, &pb.SetTagsRequest{MachineId: "machine", Tags: []string{"dev"}})
	if err != nil || !reply.Success {
		t.Fatal(err)
	}
}
func TestStopBoundsOpenStreamDrain(t *testing.T) {
	client, server := testClient(t, &testAuth{scopes: []string{"machines:read"}}, &testAuthority{})
	stream, _ := client.Sync(authorized(t.Context()))
	_ = stream.Send(&pb.SyncRequest{MachineId: "machine"})
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { server.GracefulStop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown blocked")
	}
}
func TestProductionListenerRequiresTLSAndAuthority(t *testing.T) {
	if _, err := NewService(nil, &testAuthority{}); err == nil {
		t.Fatal("accepted missing authentication")
	}
}

func TestProductionTLSListenerAuthenticates(t *testing.T) {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"sync.test"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	svc, err := NewService(&testAuth{scopes: []string{"machines:read"}}, &testAuthority{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = StartServer("127.0.0.1:0", svc, tls.Certificate{}); err == nil {
		t.Fatal("accepted cleartext configuration")
	}
	server, err := StartServer("127.0.0.1:0", svc, certificate)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Stop()
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(cert)
	conn, err := grpc.NewClient(server.Addr(), grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, ServerName: "sync.test", RootCAs: roots})))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(authorized(t.Context()), time.Second)
	defer cancel()
	stream, err := pb.NewSyncServiceClient(conn).Sync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = stream.Send(&pb.SyncRequest{MachineId: "machine"}); err != nil {
		t.Fatal(err)
	}
	reply, err := stream.Recv()
	if err != nil || len(reply.GetPeers()) != 1 {
		t.Fatalf("TLS snapshot: %v %v", reply, err)
	}
}
func TestStreamAdmissionBoundAndRecovery(t *testing.T) {
	svc, err := NewService(&testAuth{}, &testAuthority{})
	if err != nil {
		t.Fatal(err)
	}
	for range 4 {
		if !svc.acquire("session") {
			t.Fatal("early denial")
		}
	}
	if svc.acquire("session") {
		t.Fatal("unbounded session streams")
	}
	svc.release("session")
	if !svc.acquire("session") {
		t.Fatal("closed stream retained admission")
	}
	for range 4 {
		svc.release("session")
	}
	if len(svc.sessions) != 0 || svc.streams != 0 {
		t.Fatal("stream admission leak")
	}
}
