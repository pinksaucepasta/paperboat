package daemonrpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"sync"
	"time"

	pb "github.com/pinksaucepasta/paperboat/internal/daemonrpc/proto"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// DaemonBackend provides the business logic for daemon RPC operations.
type DaemonBackend interface {
	GetStatus(ctx context.Context) (*pb.StatusResponse, error)
	SubscribeStatus(ctx context.Context) (<-chan *pb.StatusResponse, func(), error)
	ResolveMachine(ctx context.Context, query string) (*pb.MachineAddress, error)
	ApprovePeer(ctx context.Context, machineID string, approved bool) error
	// Account is the signed-in account this daemon serves (id and a label to show people).
	Account() (id, label string)
}

// Server exposes the daemon RPC service over local IPC.
type Server struct {
	pb.UnimplementedDaemonServiceServer
	grpcServer *grpc.Server
	backend    DaemonBackend
	listener   net.Listener
	mu         sync.Mutex
}

// NewServer creates a new daemon RPC server instance.
func NewServer(backend DaemonBackend, opts ...grpc.ServerOption) *Server {
	opts = append([]grpc.ServerOption{grpc.ChainUnaryInterceptor(supportUnaryInterceptor), grpc.ChainStreamInterceptor(supportStreamInterceptor)}, opts...)
	s := &Server{
		grpcServer: grpc.NewServer(opts...),
		backend:    backend,
	}
	pb.RegisterDaemonServiceServer(s.grpcServer, s)
	return s
}

func incomingSupportContext(ctx context.Context) context.Context {
	values := metadata.ValueFromIncomingContext(ctx, supportReferenceMetadata)
	if len(values) == 1 && supportref.Valid(values[0]) {
		return supportref.WithContext(ctx, values[0])
	}
	return supportref.WithContext(ctx, supportref.New())
}

func supportUnaryInterceptor(ctx context.Context, request any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	return handler(incomingSupportContext(ctx), request)
}

type supportServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s supportServerStream) Context() context.Context { return s.ctx }
func supportStreamInterceptor(server any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	return handler(server, supportServerStream{ServerStream: stream, ctx: incomingSupportContext(stream.Context())})
}

type privateRPCError struct {
	grpcStatus *status.Status
	cause      error
}

func (e *privateRPCError) Error() string {
	if e == nil || e.grpcStatus == nil {
		return "daemon operation failed"
	}
	return e.grpcStatus.Message()
}

func (e *privateRPCError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *privateRPCError) GRPCStatus() *status.Status {
	if e == nil || e.grpcStatus == nil {
		return status.New(codes.Internal, "daemon operation failed")
	}
	return e.grpcStatus
}

func (e *privateRPCError) DiagnosticStage() string { return "control_request" }
func (e *privateRPCError) DiagnosticCode() string  { return "control_request_failed" }

func onlyRPCLeavesMatch(err error, expected ...error) (matched bool) {
	defer func() {
		if recover() != nil {
			matched = false
		}
	}()
	if err == nil || len(expected) == 0 {
		return false
	}
	pending := []error{err}
	leaves := 0
	for visited := 0; len(pending) > 0; visited++ {
		if visited >= 16 {
			return false
		}
		current := pending[0]
		pending = pending[1:]
		if current == nil {
			return false
		}
		value := reflect.ValueOf(current)
		if value.Kind() == reflect.Pointer && value.IsNil() {
			return false
		}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			children := wrapped.Unwrap()
			if len(children) == 0 || len(children)+len(pending) > 15-visited {
				return false
			}
			pending = append(pending, children...)
		case interface{ Unwrap() error }:
			child := wrapped.Unwrap()
			if child == nil || len(pending)+1 > 15-visited {
				return false
			}
			pending = append(pending, child)
		default:
			if !rpcLeafMatches(current, expected) {
				return false
			}
			leaves++
		}
	}
	return leaves > 0
}

func rpcLeafMatches(current error, expected []error) bool {
	for _, target := range expected {
		if reflect.TypeOf(current).Comparable() && current == target {
			return true
		}
		remote, ok := current.(interface{ GRPCStatus() *status.Status })
		if !ok {
			continue
		}
		grpcStatus := remote.GRPCStatus()
		if grpcStatus == nil {
			continue
		}
		code := grpcStatus.Code()
		if target == context.Canceled && code == codes.Canceled || target == context.DeadlineExceeded && code == codes.DeadlineExceeded {
			return true
		}
	}
	return false
}

func expectedRPCShutdown(err error) bool {
	return onlyRPCLeavesMatch(err, context.Canceled, context.DeadlineExceeded, io.EOF, io.ErrClosedPipe, net.ErrClosed)
}

func backendRPCFailure(ctx context.Context, message string, err error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if supportref.FromContext(ctx) == "" {
		ctx = supportref.WithContext(ctx, supportref.New())
	}
	code := codes.Unavailable
	safeMessage := message
	notFound := onlyRPCLeavesMatch(err, ErrMachineNotFound)
	canceled := onlyRPCLeavesMatch(err, context.Canceled, io.EOF, io.ErrClosedPipe, net.ErrClosed)
	deadline := onlyRPCLeavesMatch(err, context.DeadlineExceeded)
	switch {
	case notFound:
		code, safeMessage = codes.NotFound, "machine not found"
	case canceled:
		code, safeMessage = codes.Canceled, "daemon request canceled"
	case deadline:
		code, safeMessage = codes.DeadlineExceeded, "daemon request timed out"
	default:
		if !expectedRPCShutdown(err) {
			errorreport.Current().ObserveFailure(ctx, "paperboatd", "machine_control", "control_request", "control_request_failed", err)
		}
	}
	return &privateRPCError{
		grpcStatus: status.New(code, fmt.Sprintf("%s (support reference: %s)", safeMessage, supportref.FromContext(ctx))),
		cause:      err,
	}
}

// Serve starts the gRPC server on the provided listener.
func (s *Server) Serve(l net.Listener) error {
	s.mu.Lock()
	s.listener = l
	s.mu.Unlock()
	return s.grpcServer.Serve(l)
}

// Stop gracefully stops the server and closes the underlying listener.
func (s *Server) Stop() {
	done := make(chan struct{})
	go func() { s.grpcServer.GracefulStop(); close(done) }()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		s.grpcServer.Stop()
		<-done
	}
}

// Close immediately terminates the server.
func (s *Server) Close() {
	s.grpcServer.Stop()
}

// StreamStatus pushes live status changes to the caller.
func (s *Server) StreamStatus(_ *pb.Empty, stream pb.DaemonService_StreamStatusServer) error {
	if s.backend == nil {
		return status.Error(codes.Unavailable, "backend not configured")
	}

	ctx := stream.Context()

	updates, cancel, err := s.backend.SubscribeStatus(ctx)
	if err != nil {
		return backendRPCFailure(ctx, "daemon status is temporarily unavailable", err)
	}
	defer cancel()

	// Subscribe before reading the initial snapshot so updates cannot land in
	// the gap between the snapshot and registration.
	initial, err := s.backend.GetStatus(ctx)
	if err != nil {
		return backendRPCFailure(ctx, "daemon status is temporarily unavailable", err)
	}
	if err := stream.Send(initial); err != nil {
		return backendRPCFailure(ctx, "daemon status stream failed", err)
	}

	for {
		select {
		case <-ctx.Done():
			cause := context.Cause(ctx)
			if cause == nil {
				cause = ctx.Err()
			}
			return backendRPCFailure(ctx, "daemon status stream ended", cause)
		case update, ok := <-updates:
			if !ok {
				return nil
			}
			if err := stream.Send(update); err != nil {
				return backendRPCFailure(ctx, "daemon status stream failed", err)
			}
		}
	}
}

// AccountMismatchMessage is what a person sees when a CLI of one account reaches the local
// service of another. The request must carry the CLI's account; the daemon never serves a
// different one.
func AccountMismatchMessage(label string) string {
	return "This computer's Paperboat service is signed in as " + label + ". Use that account, or run `pb reset` to start fresh."
}

func (s *Server) requireAccount(requested string) error {
	id, label := s.backend.Account()
	if requested == "" || id == "" || requested != id {
		if label == "" {
			label = "another account"
		}
		return status.Error(codes.PermissionDenied, AccountMismatchMessage(label))
	}
	return nil
}

// ResolveMachine looks up a peer machine's internal IP, ports, and metadata.
func (s *Server) ResolveMachine(ctx context.Context, query *pb.MachineQuery) (*pb.MachineAddress, error) {
	if s.backend == nil {
		return nil, status.Error(codes.Unavailable, "backend not configured")
	}
	q := query.GetQuery()
	if q == "" {
		return nil, status.Error(codes.InvalidArgument, "query is required")
	}
	if err := s.requireAccount(query.GetAccountId()); err != nil {
		return nil, err
	}
	resp, err := s.backend.ResolveMachine(ctx, q)
	if err != nil {
		return nil, backendRPCFailure(ctx, "daemon could not resolve the requested machine", err)
	}
	return resp, nil
}

// ApprovePeer handles requests to approve or revoke peer machines.
func (s *Server) ApprovePeer(ctx context.Context, req *pb.ApprovePeerRequest) (*pb.ApprovePeerResponse, error) {
	if s.backend == nil {
		return nil, status.Error(codes.Unavailable, "backend not configured")
	}
	if req.GetMachineId() == "" {
		return nil, status.Error(codes.InvalidArgument, "machine_id is required")
	}
	if err := s.requireAccount(req.GetAccountId()); err != nil {
		return nil, err
	}
	if err := s.backend.ApprovePeer(ctx, req.GetMachineId(), req.GetApproved()); err != nil {
		var refusal Refusal
		if errors.As(err, &refusal) {
			return nil, refusalStatus(refusal)
		}
		return nil, backendRPCFailure(ctx, "daemon could not update machine approval", err)
	}
	return &pb.ApprovePeerResponse{
		MachineId: req.GetMachineId(),
		Approved:  req.GetApproved(),
		Success:   true,
	}, nil
}
