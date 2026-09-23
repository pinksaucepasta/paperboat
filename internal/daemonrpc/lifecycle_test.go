package daemonrpc

import (
	"context"
	"sync"
	"testing"
	"time"

	pbSync "github.com/pinksaucepasta/paperboat/internal/controlsync/proto"
)

func TestBrowserURLsFollowOwnedSnapshot(t *testing.T) {
	b := NewLiveDaemonBackend(nil, "test")
	peer := &pbSync.PeerUpdate{PeerId: "device", Alias: "office", Approved: true, Online: true, ExportedPorts: []int32{3000}}
	urls := map[string]map[int32]string{"device": {3000: "https://0123456789abcdef.pprbt"}}
	b.ApplyPeerUpdates([]*pbSync.PeerUpdate{peer}, 1, urls)
	urls["device"][3000] = "mutated"
	resolved, err := b.ResolveDevice(t.Context(), "device")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.BrowserUrls[3000] != "https://0123456789abcdef.pprbt" {
		t.Fatal("snapshot retained caller-owned URL map")
	}
	resolved.BrowserUrls[3000] = "mutated"
	status, err := b.GetStatus(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if status.Peers[0].BrowserUrls[3000] != "https://0123456789abcdef.pprbt" {
		t.Fatal("resolver exposed mutable URL map")
	}
	b.ApplyPeerUpdates(nil, 2, nil)
	status, err = b.GetStatus(t.Context())
	if err != nil || len(status.Peers) != 0 {
		t.Fatal("withdrawal retained browser URLs", err)
	}
}

func TestSnapshotWithdrawalAndConcurrentSubscriberCancellation(t *testing.T) {
	b := NewLiveDaemonBackend(nil, "test")
	peer := &pbSync.PeerUpdate{PeerId: "machine_a", Alias: "alpha", AssignedIp: "127.100.0.2", Approved: true, Online: true}
	b.ApplyPeerUpdates([]*pbSync.PeerUpdate{peer}, 1, nil)
	if _, err := b.ResolveDevice(context.Background(), "alpha"); err != nil {
		t.Fatal(err)
	}
	b.ApplyPeerUpdates(nil, 2, nil)
	if _, err := b.ResolveDevice(context.Background(), "alpha"); err == nil {
		t.Fatal("withdrawn peer remained resolvable")
	}
	peer.Approved = false
	b.ApplyPeerUpdates([]*pbSync.PeerUpdate{peer}, 3, nil)
	if _, err := b.ResolveDevice(context.Background(), "alpha"); err == nil {
		t.Fatal("unapproved peer remained resolvable")
	}
	var wg sync.WaitGroup
	for n := 0; n < 8; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				_, cancel, err := b.SubscribeStatus(context.Background())
				if err != nil {
					t.Error(err)
					return
				}
				b.ApplyPeerUpdates(nil, uint64(i+4), nil)
				cancel()
				cancel()
			}
		}()
	}
	wg.Wait()
	ch, cancel, _ := b.SubscribeStatus(context.Background())
	defer cancel()
	for i := 0; i < 100; i++ {
		b.ApplyPeerUpdates(nil, uint64(i+1), nil)
	}
	b.ApplyPeerUpdates(nil, 0, nil)
	var last uint64
	for len(ch) > 0 {
		last = (<-ch).Generation
	}
	if last != 0 {
		t.Fatal("slow subscriber missed final withdrawal", last)
	}
	if err := b.SetDeviceTags(context.Background(), "machine_a", []string{"tag"}); err == nil {
		t.Fatal("unconfigured mutation reported success")
	}
	if err := b.ApprovePeer(context.Background(), "machine_a", true); err == nil {
		t.Fatal("unsigned approval reported success")
	}
}

func TestServerStopBoundsLiveStream(t *testing.T) {
	socket := testSocket(t)
	listener, err := Listen(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server := NewServer(NewLiveDaemonBackend(nil, "test"))
	go func() { _ = server.Serve(listener) }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := NewClient(ctx, socket)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	stream, err := client.StreamStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = stream.Recv(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { server.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown blocked by live status subscriber")
	}
}
