// Package sync projects existing authenticated control-plane authority to local daemons.
package sync

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	serverdiagnostics "github.com/pinksaucepasta/paperboat/server/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/server/internal/supportreport"
	pb "github.com/pinksaucepasta/paperboat/server/internal/sync/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const RefreshInterval = 5 * time.Second
const RequestTimeout = 5 * time.Second

type Authenticator interface {
	Authenticate(context.Context, string) (auth.ClientPrincipal, error)
}
type Authority interface {
	Snapshot(context.Context, auth.ClientPrincipal, string) (*pb.SyncResponse, error)
	SetTags(context.Context, auth.ClientPrincipal, string, []string) error
}
type Service struct {
	pb.UnimplementedSyncServiceServer
	auth      Authenticator
	authority Authority
	reporter  *supportreport.Service
	interval  time.Duration
	ioTimeout time.Duration
	mu        sync.Mutex
	sessions  map[string]int
	streams   int
}

func NewService(authenticator Authenticator, authority Authority) (*Service, error) {
	if authenticator == nil || authority == nil {
		return nil, errors.New("sync authentication and authority are required")
	}
	return &Service{auth: authenticator, authority: authority, interval: RefreshInterval, ioTimeout: RequestTimeout, sessions: make(map[string]int)}, nil
}

// SetSupportReporter binds the request-local support reporter before the
// service is registered and begins serving concurrent RPCs.
func (s *Service) SetSupportReporter(reporter *supportreport.Service) {
	if s != nil {
		s.reporter = reporter
	}
}

func (s *Service) Register(server *grpc.Server) { pb.RegisterSyncServiceServer(server, s) }

func (s *Service) requestContext(ctx context.Context) (context.Context, string) {
	if ctx == nil {
		ctx = context.Background()
	}
	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get("support-reference")
	reference := ""
	if len(values) == 1 && supportreport.ValidReference(values[0]) {
		reference = values[0]
	} else {
		reference = supportreport.NewReference()
	}
	ctx = serverdiagnostics.WithCorrelationID(ctx, reference)
	if s.reporter != nil {
		ctx = s.reporter.Context(ctx, reference)
	}
	return ctx, reference
}

func (s *Service) principal(ctx context.Context, scope string) (auth.ClientPrincipal, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get("authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") || len(values[0]) > 8192 {
		return auth.ClientPrincipal{}, status.Error(codes.Unauthenticated, "CLI access token required")
	}
	token := strings.TrimPrefix(values[0], "Bearer ")
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return auth.ClientPrincipal{}, status.Error(codes.Unauthenticated, "invalid CLI access token")
	}
	p, err := s.auth.Authenticate(ctx, token)
	if err != nil {
		return p, err
	}
	if !p.HasScope(scope) {
		return p, status.Error(codes.PermissionDenied, "CLI session lacks required scope")
	}
	return p, nil
}

// Sync responses are complete replacements, never lossy deltas. No connection owns
// database admission or leases, so closing an older stream cannot revoke its successor.
func (s *Service) Sync(stream pb.SyncService_SyncServer) error {
	ctx, reference := s.requestContext(stream.Context())
	checkCtx, cancel := context.WithTimeout(ctx, RequestTimeout)
	principal, err := s.principal(checkCtx, "machines:read")
	if err != nil {
		err = withContextCause(checkCtx, err)
	}
	cancel()
	if err != nil {
		return s.rpcError(ctx, reference, principal.User.ID, err)
	}
	ctx = supportreport.WithFailureIdentity(ctx, principal.User.ID, "", reference)
	owner := principal.User.ID
	if !s.acquire(principal.SessionID) {
		return status.Error(codes.ResourceExhausted, "sync stream limit reached; close older connections")
	}
	defer s.release(principal.SessionID)
	var request *pb.SyncRequest
	err = boundedStreamIO(ctx, s.ioTimeout, func() error { var recvErr error; request, recvErr = stream.Recv(); return recvErr })
	if err == io.EOF {
		return nil
	}
	if err != nil {
		return s.rpcError(ctx, reference, principal.User.ID, err)
	}
	if len(request.GetMachineId()) > 128 {
		return status.Error(codes.InvalidArgument, "invalid source machine")
	}
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	var revision uint64
	for {
		callCtx, stop := context.WithTimeout(ctx, RequestTimeout)
		currentPrincipal, err := s.principal(callCtx, "machines:read")
		var snapshot *pb.SyncResponse
		if err == nil {
			owner = currentPrincipal.User.ID
			snapshot, err = s.authority.Snapshot(callCtx, currentPrincipal, request.GetMachineId())
		}
		if err != nil {
			err = withContextCause(callCtx, err)
		}
		stop()
		if err != nil {
			return s.rpcError(ctx, reference, owner, err)
		}
		revision++
		snapshot.Revision = revision
		if err = boundedStreamIO(ctx, s.ioTimeout, func() error { return stream.Send(snapshot) }); err != nil {
			return s.rpcError(ctx, reference, owner, err)
		}
		select {
		case <-ctx.Done():
			return s.rpcError(ctx, reference, owner, withContextCause(ctx, ctx.Err()))
		case <-ticker.C:
		}
	}
}
func (s *Service) SetTags(ctx context.Context, req *pb.SetTagsRequest) (*pb.SetTagsResponse, error) {
	ctx, reference := s.requestContext(ctx)
	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	p, err := s.principal(ctx, "machines:connect")
	if err != nil {
		return nil, s.rpcError(ctx, reference, p.User.ID, withContextCause(ctx, err))
	}
	ctx = supportreport.WithFailureIdentity(ctx, p.User.ID, "", reference)
	if req.GetMachineId() == "" || len(req.GetMachineId()) > 128 || len(req.GetTags()) > 32 {
		return nil, status.Error(codes.InvalidArgument, "invalid machine tags")
	}
	seen := map[string]bool{}
	for _, tag := range req.GetTags() {
		if len(tag) == 0 || len(tag) > 64 || strings.TrimSpace(tag) != tag || strings.ContainsAny(tag, "\x00\r\n\t") || seen[tag] {
			return nil, status.Error(codes.InvalidArgument, "invalid machine tags")
		}
		seen[tag] = true
	}
	if err = s.authority.SetTags(ctx, p, req.GetMachineId(), req.GetTags()); err != nil {
		return nil, s.rpcError(ctx, reference, p.User.ID, withContextCause(ctx, err))
	}
	return &pb.SetTagsResponse{MachineId: req.MachineId, Tags: req.Tags, Success: true}, nil
}
func (s *Service) rpcError(ctx context.Context, reference, owner string, err error) error {
	if err == nil {
		return nil
	}
	code, pure := pureRPCErrorCode(err)
	if pure {
		if code == codes.Canceled || expectedRPCRejection(code) {
			return &privateRPCError{st: status.New(code, safeRPCMessage(code, "")), cause: err}
		}
	}

	publicCode := codes.Unavailable
	if pure && code == codes.DeadlineExceeded {
		publicCode = codes.DeadlineExceeded
	}
	faultCtx := supportreport.WithFailureIdentity(ctx, owner, "", reference)
	fault := supportreport.CaptureFromContext(faultCtx, "paperboat-server", "service_lifecycle", "service_error", "control_sync_failed", err)
	if supportreport.ValidReference(fault.SupportReference) {
		reference = fault.SupportReference
	}
	if supportreport.ValidReference(reference) {
		_ = grpc.SetTrailer(ctx, metadata.Pairs("support-reference", reference))
	}
	return &privateRPCError{
		st:    status.New(publicCode, safeRPCMessage(publicCode, reference)),
		cause: err,
	}
}

func expectedRPCRejection(code codes.Code) bool {
	switch code {
	case codes.Unauthenticated, codes.PermissionDenied, codes.ResourceExhausted:
		return true
	default:
		return false
	}
}

func safeRPCMessage(code codes.Code, reference string) string {
	message := "control-plane state unavailable; reconnect"
	switch code {
	case codes.Canceled:
		message = "sync canceled"
	case codes.DeadlineExceeded:
		message = "control-plane lookup timed out; reconnect"
	case codes.Unauthenticated:
		message = "CLI session expired or revoked; refresh credentials and reconnect"
	case codes.PermissionDenied:
		message = "sync access denied"
	case codes.ResourceExhausted:
		message = "sync resource limit reached; close older connections or remove retired machines"
	}
	if supportreport.ValidReference(reference) {
		message += "; support reference " + reference
	}
	return message
}

// pureRPCErrorCode accepts only a complete, bounded tree of known terminal
// statuses or cancellation sentinels. Joined operational causes therefore
// cannot be downgraded by an expected status or cancellation sibling.
func pureRPCErrorCode(err error) (codes.Code, bool) {
	if err == nil {
		return codes.OK, false
	}
	grpcErrorType := reflect.TypeOf(status.Error(codes.Internal, ""))
	pending := []error{err}
	var result codes.Code
	found := false
	for visited := 0; len(pending) > 0 && visited < 32; visited++ {
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if current == nil || nilRPCError(current) {
			return codes.OK, false
		}
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			children := joined.Unwrap()
			if len(children) == 0 {
				return codes.OK, false
			}
			pending = append(pending, children...)
			continue
		}
		if wrapped, ok := current.(interface{ Unwrap() error }); ok {
			child := wrapped.Unwrap()
			if child == nil {
				return codes.OK, false
			}
			pending = append(pending, child)
			continue
		}

		var code codes.Code
		switch current {
		case context.Canceled:
			code = codes.Canceled
		case context.DeadlineExceeded:
			code = codes.DeadlineExceeded
		case auth.ErrUnauthenticated:
			code = codes.Unauthenticated
		default:
			if reflect.TypeOf(current) != grpcErrorType {
				return codes.OK, false
			}
			st, ok := status.FromError(current)
			if !ok || st == nil || st.Code() == codes.OK {
				return codes.OK, false
			}
			code = st.Code()
		}
		if found && result != code {
			return codes.OK, false
		}
		result, found = code, true
	}
	if len(pending) != 0 || !found {
		return codes.OK, false
	}
	return result, true
}

func nilRPCError(err error) bool {
	value := reflect.ValueOf(err)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func withContextCause(ctx context.Context, err error) error {
	if ctx == nil || ctx.Err() == nil {
		return err
	}
	cause := context.Cause(ctx)
	if cause == nil {
		cause = ctx.Err()
	}
	if err == nil {
		return errors.Join(ctx.Err(), cause)
	}
	errCode, errPure := pureRPCErrorCode(err)
	causeCode, causePure := pureRPCErrorCode(cause)
	if errPure && causePure && errCode == causeCode {
		return err
	}
	return errors.Join(err, cause)
}

type privateRPCError struct {
	st    *status.Status
	cause error
}

func (e *privateRPCError) Error() string              { return e.st.Message() }
func (e *privateRPCError) Unwrap() error              { return e.cause }
func (e *privateRPCError) GRPCStatus() *status.Status { return e.st }

// Counts are admission bounds only; streams never own shared topology or leases.
func (s *Service) acquire(session string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.streams >= 256 || s.sessions[session] >= 4 {
		return false
	}
	s.streams++
	s.sessions[session]++
	return true
}
func (s *Service) release(session string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streams--
	s.sessions[session]--
	if s.sessions[session] == 0 {
		delete(s.sessions, session)
	}
}

// Returning from the RPC handler cancels its gRPC transport and releases any
// blocked Recv/Send. The buffered completion channel never retains a worker.
func boundedStreamIO(ctx context.Context, timeout time.Duration, call func() error) error {
	if timeout <= 0 {
		timeout = RequestTimeout
	}
	done := make(chan error, 1)
	go func() { done <- call() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return withContextCause(ctx, err)
	case <-ctx.Done():
		return withContextCause(ctx, ctx.Err())
	case <-timer.C:
		if ctx.Err() != nil {
			return withContextCause(ctx, ctx.Err())
		}
		return status.Error(codes.DeadlineExceeded, "sync stream stalled; reconnect")
	}
}
