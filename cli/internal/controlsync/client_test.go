package controlsync

import (
	"context"
	"errors"
	"fmt"
	pb "github.com/pinksaucepasta/paperboat/internal/controlsync/proto"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type snapshotTestServer struct {
	pb.UnimplementedSyncServiceServer
	calls   atomic.Int32
	drop    chan struct{}
	badAuth atomic.Bool
	refsMu  sync.Mutex
	refs    []string
}

type syncContextCause struct{}

func (syncContextCause) Error() string { return "private parent cancellation cause" }

func (s *snapshotTestServer) Sync(stream pb.SyncService_SyncServer) error {
	if _, err := stream.Recv(); err != nil {
		return err
	}
	md, _ := metadata.FromIncomingContext(stream.Context())
	if len(md.Get("authorization")) != 1 || md.Get("authorization")[0] != "Bearer test-token" {
		s.badAuth.Store(true)
	}
	s.recordReference(md)
	call := s.calls.Add(1)
	if call == 2 {
		return status.Error(codes.Unavailable, "private transient server detail")
	}
	peers := []*pb.PeerUpdate{{PeerId: "live", Approved: true, Online: true, AssignedIp: "127.100.0.2"}}
	if call == 1 {
		peers = append(peers, &pb.PeerUpdate{PeerId: "removed", Approved: true, Online: true})
	}
	if err := stream.Send(&pb.SyncResponse{Revision: 1, Peers: peers}); err != nil {
		return err
	}
	if call == 1 {
		select {
		case <-s.drop:
			return status.Error(codes.Unavailable, "private transient server detail")
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
	}
	<-stream.Context().Done()
	return stream.Context().Err()
}

func (s *snapshotTestServer) recordReference(md metadata.MD) {
	s.refsMu.Lock()
	s.refs = append(s.refs, md.Get(supportref.Header)...)
	s.refsMu.Unlock()
}

func TestSnapshotRecoveryWithdrawsAndReplaces(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	backend := &snapshotTestServer{drop: make(chan struct{})}
	pb.RegisterSyncServiceServer(server, backend)
	go server.Serve(listener)
	defer server.Stop()
	updates := make(chan []*pb.PeerUpdate, 16)
	faults := make(chan errorreport.Fault, 4)
	restoreReporter := errorreport.Install(&errorreport.Reporter{})
	defer restoreReporter()
	restoreFaultObserver := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) { faults <- fault })
	defer restoreFaultObserver()
	client := NewClient(ClientConfig{ServerAddr: listener.Addr().String(), Insecure: true, AuthToken: "test-token", OnPeerUpdate: func(peers []*pb.PeerUpdate, _ uint64) { updates <- peers }})
	const reference = "support_01234567-89ab-4def-8123-456789abcdef"
	parentCtx, cancelCause := context.WithCancelCause(supportref.WithContext(context.Background(), reference))
	defer cancelCause(context.Canceled)
	ctx, cancelTimeout := context.WithTimeout(parentCtx, 5*time.Second)
	defer cancelTimeout()
	if err = client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if len(<-updates) != 2 {
		t.Fatal("missing initial snapshot")
	}
	snapshot := client.Peers()
	snapshot[0].PeerId = "mutated"
	if client.Peers()[0].PeerId == "mutated" {
		t.Fatal("snapshot exposed mutable state")
	}
	close(backend.drop)
	select {
	case peers := <-updates:
		if len(peers) != 0 {
			t.Fatal("disconnect did not withdraw peers")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case peers := <-updates:
		if len(peers) != 1 || peers[0].PeerId != "live" {
			t.Fatal("reconnect did not replace full snapshot")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if backend.badAuth.Load() {
		t.Fatal("missing authenticated metadata")
	}
	backend.refsMu.Lock()
	refs := append([]string(nil), backend.refs...)
	backend.refsMu.Unlock()
	if len(refs) != 3 {
		t.Fatalf("support-reference metadata on RPCs = %v", refs)
	}
	for _, got := range refs {
		if got != reference {
			t.Fatalf("support-reference metadata = %q, want %q", got, reference)
		}
	}
	select {
	case fault := <-faults:
		if fault.Code != "config_sync_failed" || fault.SupportReference != reference {
			t.Fatalf("fault projection = %+v", fault)
		}
		if strings.Contains(fmt.Sprint(fault), "private transient server detail") {
			t.Fatalf("private server detail escaped projection: %+v", fault)
		}
	default:
		t.Fatal("transient background failure was not observed")
	}
	if len(faults) != 0 {
		t.Fatalf("retry episode reported more than once: %d faults", len(faults)+1)
	}
	cancelCause(syncContextCause{})
	select {
	case <-client.done:
	case <-time.After(time.Second):
		t.Fatal("parent cancellation did not stop control sync")
	}
	select {
	case fault := <-faults:
		causeRetained := false
		for _, name := range fault.ErrorChain {
			if name == "syncContextCause" {
				causeRetained = true
			}
		}
		if fault.Code != "config_sync_failed" || fault.SupportReference != reference || !causeRetained || strings.Contains(fmt.Sprint(fault), "private parent cancellation cause") {
			t.Fatalf("parent cancellation cause was lost or exposed: %+v", fault)
		}
	default:
		t.Fatal("custom parent cancellation cause was not observed")
	}
	if err = client.Close(); err != nil {
		t.Fatal(err)
	}
	if len(client.Peers()) != 0 {
		t.Fatal("close retained authority")
	}
}
func TestInsecureRemoteRejected(t *testing.T) {
	c := NewClient(ClientConfig{ServerAddr: "192.0.2.1:443", Insecure: true})
	if err := c.Start(context.Background()); err == nil {
		c.Close()
		t.Fatal("allowed cleartext remote sync")
	}
}

func TestTerminalSyncErrorPreservesOnlyPureTerminalCauses(t *testing.T) {
	original := status.Error(codes.PermissionDenied, "private server detail")
	failure := terminalSyncError(fmt.Errorf("stream failed: %w", original))
	if failure == nil || status.Code(failure) != codes.PermissionDenied || !errors.Is(failure, original) {
		t.Fatalf("terminal failure lost status or original cause: %v", failure)
	}
	if strings.Contains(failure.Error(), "private server detail") {
		t.Fatalf("terminal failure exposed private status text: %v", failure)
	}
	if mixed := terminalSyncError(errors.Join(original, errors.New("independent stream failure"))); mixed != nil {
		t.Fatalf("mixed failure was treated as pure terminal auth: %v", mixed)
	}
}
