package controlsync

import (
	"context"
	pb "github.com/pinksaucepasta/paperboat/internal/controlsync/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

type snapshotTestServer struct {
	pb.UnimplementedSyncServiceServer
	calls   atomic.Int32
	drop    chan struct{}
	badAuth atomic.Bool
}

func (s *snapshotTestServer) Sync(stream pb.SyncService_SyncServer) error {
	if _, err := stream.Recv(); err != nil {
		return err
	}
	md, _ := metadata.FromIncomingContext(stream.Context())
	if len(md.Get("authorization")) != 1 || md.Get("authorization")[0] != "Bearer test-token" {
		s.badAuth.Store(true)
	}
	call := s.calls.Add(1)
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
			return nil
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
	}
	<-stream.Context().Done()
	return stream.Context().Err()
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
	client := NewClient(ClientConfig{ServerAddr: listener.Addr().String(), Insecure: true, AuthToken: "test-token", OnPeerUpdate: func(peers []*pb.PeerUpdate, _ uint64) { updates <- peers }})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
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
