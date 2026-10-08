package daemonrpc

import (
	"context"
	"net"
	"sync"
	"time"

	pb "github.com/pinksaucepasta/paperboat/internal/daemonrpc/proto"
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
	SetMachineTags(ctx context.Context, machineID string, tags []string) error
	ApprovePeer(ctx context.Context, machineID string, approved bool) error
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
	return ctx
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

	// Push the initial status immediately
	initial, err := s.backend.GetStatus(ctx)
	if err != nil {
		return status.Errorf(codes.Internal, "failed to get initial status: %v", err)
	}
	if err := stream.Send(initial); err != nil {
		return err
	}

	updates, cancel, err := s.backend.SubscribeStatus(ctx)
	if err != nil {
		return status.Errorf(codes.Internal, "failed to subscribe to status updates: %v", err)
	}
	defer cancel()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case update, ok := <-updates:
			if !ok {
				return nil
			}
			if err := stream.Send(update); err != nil {
				return err
			}
		}
	}
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
	resp, err := s.backend.ResolveMachine(ctx, q)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "resolve machine failed: %v", err)
	}
	return resp, nil
}

// SetMachineTags sets or updates user-defined metadata tags for a machine.
func (s *Server) SetMachineTags(ctx context.Context, req *pb.SetMachineTagsRequest) (*pb.SetMachineTagsResponse, error) {
	if s.backend == nil {
		return nil, status.Error(codes.Unavailable, "backend not configured")
	}
	if req.GetMachineId() == "" {
		return nil, status.Error(codes.InvalidArgument, "machine_id is required")
	}
	if err := s.backend.SetMachineTags(ctx, req.GetMachineId(), req.GetTags()); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to set machine tags: %v", err)
	}
	return &pb.SetMachineTagsResponse{
		MachineId: req.GetMachineId(),
		Tags:      req.GetTags(),
		Success:   true,
	}, nil
}

// ApprovePeer handles requests to approve or revoke peer machines.
func (s *Server) ApprovePeer(ctx context.Context, req *pb.ApprovePeerRequest) (*pb.ApprovePeerResponse, error) {
	if s.backend == nil {
		return nil, status.Error(codes.Unavailable, "backend not configured")
	}
	if req.GetMachineId() == "" {
		return nil, status.Error(codes.InvalidArgument, "machine_id is required")
	}
	if err := s.backend.ApprovePeer(ctx, req.GetMachineId(), req.GetApproved()); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to approve peer: %v", err)
	}
	return &pb.ApprovePeerResponse{
		MachineId: req.GetMachineId(),
		Approved:  req.GetApproved(),
		Success:   true,
	}, nil
}
