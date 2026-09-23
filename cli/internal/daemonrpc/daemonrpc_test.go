package daemonrpc

import (
	"context"
	"fmt"
	"io"
	"os"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

	pbSync "github.com/pinksaucepasta/paperboat/internal/controlsync/proto"
	pb "github.com/pinksaucepasta/paperboat/internal/daemonrpc/proto"
)

type mockBackend struct {
	mu          sync.Mutex
	status      *pb.StatusResponse
	subscribers []chan *pb.StatusResponse
	devices     map[string]*pb.DeviceAddress
	deviceTags  map[string][]string
}

func newMockBackend() *mockBackend {
	return &mockBackend{
		status: &pb.StatusResponse{
			DaemonState:    "running",
			DaemonVersion:  "test-v1",
			Generation:     1,
			ObservedAtUnix: time.Now().Unix(),
			Peers: []*pb.PeerStatus{
				{
					PeerId:         "dev-1",
					Alias:          "workstation",
					AssignedIp:     "127.100.0.2",
					Online:         true,
					LatencyMs:      15,
					ConnectionMode: "direct",
					Tags:           []string{"work", "fast"},
				},
			},
			DerpRelays: []*pb.DERPStatus{
				{
					RegionCode: "fra",
					RegionName: "Frankfurt",
					Reachable:  true,
					LatencyMs:  22,
				},
			},
		},
		devices: map[string]*pb.DeviceAddress{
			"workstation": {
				DeviceId:       "dev-1",
				Alias:          "workstation",
				AssignedIp:     "127.100.0.2",
				ForwardedPorts: []int32{22, 8080},
				Tags:           []string{"work", "fast"},
			},
		},
		deviceTags: make(map[string][]string),
	}
}

func (m *mockBackend) GetStatus(ctx context.Context) (*pb.StatusResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status, nil
}

func (m *mockBackend) SubscribeStatus(ctx context.Context) (<-chan *pb.StatusResponse, func(), error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ch := make(chan *pb.StatusResponse, 10)
	m.subscribers = append(m.subscribers, ch)
	cancel := func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		for i, sub := range m.subscribers {
			if sub == ch {
				m.subscribers = append(m.subscribers[:i], m.subscribers[i+1:]...)
				close(ch)
				break
			}
		}
	}
	return ch, cancel, nil
}

func (m *mockBackend) broadcastStatus(update *pb.StatusResponse) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status = update
	for _, sub := range m.subscribers {
		select {
		case sub <- update:
		default:
		}
	}
}

func (m *mockBackend) ResolveDevice(ctx context.Context, query string) (*pb.DeviceAddress, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if dev, ok := m.devices[query]; ok {
		tags := m.deviceTags[dev.DeviceId]
		if len(tags) > 0 {
			dev.Tags = tags
		}
		return dev, nil
	}
	return nil, fmt.Errorf("device %q not found", query)
}

func (m *mockBackend) ApprovePeer(ctx context.Context, deviceID string, approved bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return nil
}

func (m *mockBackend) SetDeviceTags(ctx context.Context, deviceID string, tags []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deviceTags[deviceID] = tags
	return nil
}

func TestDaemonRPCServerAndClient(t *testing.T) {
	sockPath := testSocket(t)

	listener, err := Listen(sockPath)
	if err != nil {
		t.Fatalf("Listen() error: %v", err)
	}
	defer listener.Close()

	if runtime.GOOS != "windows" {
		fi, err := os.Stat(sockPath)
		if err != nil {
			t.Fatalf("stat socket error: %v", err)
		}
		if perm := fi.Mode().Perm(); perm != 0600 {
			t.Fatalf("expected socket perms 0600, got %#o", perm)
		}
	}

	backend := newMockBackend()
	server := NewServer(backend)

	go func() {
		_ = server.Serve(listener)
	}()
	defer server.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := NewClient(ctx, sockPath)
	if err != nil {
		t.Fatalf("NewClient() error: %v", err)
	}
	defer client.Close()

	// 1. Test ResolveDevice
	dev, err := client.ResolveDevice(ctx, "workstation")
	if err != nil {
		t.Fatalf("ResolveDevice() error: %v", err)
	}
	if dev.DeviceId != "dev-1" || dev.AssignedIp != "127.100.0.2" {
		t.Fatalf("unexpected resolved device: %+v", dev)
	}

	// 2. Test SetDeviceTags and then resolve again
	tagResp, err := client.SetDeviceTags(ctx, "dev-1", []string{"production", "gateway"})
	if err != nil {
		t.Fatalf("SetDeviceTags() error: %v", err)
	}
	if !tagResp.Success || !reflect.DeepEqual(tagResp.Tags, []string{"production", "gateway"}) {
		t.Fatalf("unexpected SetDeviceTags response: %+v", tagResp)
	}

	dev, err = client.ResolveDevice(ctx, "workstation")
	if err != nil {
		t.Fatalf("ResolveDevice after tags error: %v", err)
	}
	if !reflect.DeepEqual(dev.Tags, []string{"production", "gateway"}) {
		t.Fatalf("expected updated tags, got %v", dev.Tags)
	}

	// 4. Test StreamStatus
	stream, err := client.StreamStatus(ctx)
	if err != nil {
		t.Fatalf("StreamStatus() error: %v", err)
	}

	// Initial message
	initMsg, err := stream.Recv()
	if err != nil {
		t.Fatalf("stream.Recv() initial error: %v", err)
	}
	if initMsg.DaemonState != "running" || len(initMsg.Peers) != 1 {
		t.Fatalf("unexpected initial status: %+v", initMsg)
	}

	// Broadcast an update
	updatedStatus := &pb.StatusResponse{
		DaemonState:    "connected",
		DaemonVersion:  "test-v1",
		Generation:     2,
		ObservedAtUnix: time.Now().Unix(),
		Peers: []*pb.PeerStatus{
			{
				PeerId:     "dev-1",
				Alias:      "workstation",
				AssignedIp: "127.100.0.2",
				Online:     true,
				LatencyMs:  12,
			},
			{
				PeerId:     "dev-2",
				Alias:      "laptop",
				AssignedIp: "127.100.0.3",
				Online:     true,
				LatencyMs:  30,
			},
		},
	}
	backend.broadcastStatus(updatedStatus)

	updateMsg, err := stream.Recv()
	if err != nil && err != io.EOF {
		t.Fatalf("stream.Recv() update error: %v", err)
	}
	if updateMsg.Generation != 2 || len(updateMsg.Peers) != 2 {
		t.Fatalf("unexpected updated status: %+v", updateMsg)
	}
}

func TestDefaultSocketAddress(t *testing.T) {
	addr := DefaultSocketAddress()
	if addr == "" {
		t.Fatalf("DefaultSocketAddress() returned empty string")
	}
}

func TestLiveDaemonBackendAuthorizedPortsAndUnknownTransport(t *testing.T) {
	backend := NewLiveDaemonBackend(nil, "v2.0.0")

	// Ingest peer update with dynamic ports
	updates := []*pbSync.PeerUpdate{
		{
			PeerId:        "node-xyz",
			Alias:         "dev-box",
			AssignedIp:    "127.100.0.5",
			Online:        true,
			Approved:      true,
			ExportedPorts: []int32{3000, 8080, 5432},
			Tags:          []string{"backend", "staging"},
		},
	}
	backend.ApplyPeerUpdates(updates, 10, nil)

	// 1. Resolve device and verify exported ports are dynamic, not static
	addr, err := backend.ResolveDevice(context.Background(), "dev-box")
	if err != nil {
		t.Fatalf("ResolveDevice failed: %v", err)
	}
	if addr.DeviceId != "node-xyz" || addr.AssignedIp != "127.100.0.5" {
		t.Fatalf("unexpected device address: %+v", addr)
	}
	if !reflect.DeepEqual(addr.ForwardedPorts, []int32{3000, 8080, 5432}) {
		t.Fatalf("expected dynamic ports [3000, 8080, 5432], got %v", addr.ForwardedPorts)
	}

	// 2. Topology does not establish measured transport latency or mode.
	status, err := backend.GetStatus(context.Background())
	if err != nil {
		t.Fatalf("GetStatus failed: %v", err)
	}
	if len(status.Peers) != 1 {
		t.Fatalf("expected 1 peer, got %d", len(status.Peers))
	}
	p := status.Peers[0]
	if p.LatencyMs != 0 || p.ConnectionMode != "unknown" {
		t.Fatalf("expected no invented latency or transport mode, got %dms and %s", p.LatencyMs, p.ConnectionMode)
	}
	if !reflect.DeepEqual(p.ForwardedPorts, []int32{3000, 8080, 5432}) {
		t.Fatalf("expected status forwarded ports [3000, 8080, 5432], got %v", p.ForwardedPorts)
	}
}
