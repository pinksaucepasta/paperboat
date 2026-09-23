package controlsync

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/pinksaucepasta/paperboat/internal/controlsync/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type rejectedSyncServer struct {
	pb.UnimplementedSyncServiceServer
	calls atomic.Int32
}

func (s *rejectedSyncServer) Sync(stream pb.SyncService_SyncServer) error {
	if _, err := stream.Recv(); err != nil {
		return err
	}
	if s.calls.Add(1) == 1 {
		if err := stream.Send(&pb.SyncResponse{Revision: 1, Peers: []*pb.PeerUpdate{{PeerId: "device", Approved: true}}}); err != nil {
			return err
		}
	}
	return status.Error(codes.PermissionDenied, "fixture rejection")
}
func TestTerminalSyncErrorWithdrawsAndStopsRetries(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	backend := &rejectedSyncServer{}
	pb.RegisterSyncServiceServer(server, backend)
	go server.Serve(listener)
	defer server.Stop()
	failures := make(chan error, 1)
	client := NewClient(ClientConfig{ServerAddr: listener.Addr().String(), Insecure: true, OnError: func(err error) { failures <- err }})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err = client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	select {
	case err = <-failures:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if status.Code(err) != codes.PermissionDenied || len(client.Peers()) != 0 {
		t.Fatalf("failure=%v peers=%v", err, client.Peers())
	}
	select {
	case <-client.done:
	case <-ctx.Done():
		t.Fatal("retry loop retained after terminal failure")
	}
	if backend.calls.Load() != 1 {
		t.Fatalf("terminal denial retried %d times", backend.calls.Load())
	}
}
func TestRetryBackoffBoundAndReset(t *testing.T) {
	b := newRetryBackoff()
	first := b.NextBackOff()
	if first < 400*time.Millisecond || first > 1200*time.Millisecond {
		t.Fatalf("first delay=%s", first)
	}
	for range 40 {
		if delay := b.NextBackOff(); delay < 400*time.Millisecond || delay > 9*time.Second {
			t.Fatalf("unbounded delay=%s", delay)
		}
	}
	b.Reset()
	if delay := b.NextBackOff(); delay < 400*time.Millisecond || delay > 1200*time.Millisecond {
		t.Fatalf("reset delay=%s", delay)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := waitRetry(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
func TestSyncErrorClassification(t *testing.T) {
	for _, code := range []codes.Code{codes.Unauthenticated, codes.PermissionDenied, codes.InvalidArgument, codes.FailedPrecondition, codes.Unimplemented} {
		if err := terminalSyncError(status.Error(code, "private server text")); status.Code(err) != code {
			t.Fatalf("code %s became %v", code, err)
		}
	}
	for _, err := range []error{status.Error(codes.Unavailable, "outage"), status.Error(codes.ResourceExhausted, "busy"), context.DeadlineExceeded} {
		if terminalSyncError(err) != nil {
			t.Fatalf("transient classified permanent: %v", err)
		}
	}
}
