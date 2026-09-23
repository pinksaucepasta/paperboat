package daemonrpc

import (
	"context"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/controlsync"
	pbSync "github.com/pinksaucepasta/paperboat/internal/controlsync/proto"
	pb "github.com/pinksaucepasta/paperboat/internal/daemonrpc/proto"
)

// LiveDaemonBackend connects the daemon's control sync client to the local IPC gRPC service.
type LiveDaemonBackend struct {
	mu          sync.RWMutex
	syncClient  *controlsync.Client
	version     string
	state       string
	generation  uint64
	peers       map[string]*pb.PeerStatus
	subscribers []chan *pb.StatusResponse
	approve     func(context.Context, string, bool) error
}

// NewLiveDaemonBackend creates a DaemonBackend backed by a real control plane sync client.
func NewLiveDaemonBackend(syncClient *controlsync.Client, version string) *LiveDaemonBackend {
	b := &LiveDaemonBackend{
		syncClient: syncClient,
		version:    version,
		state:      "disconnected",
		generation: 0,
		peers:      make(map[string]*pb.PeerStatus),
	}
	return b
}

// ApplyPeerUpdates ingests peer updates received from the control plane sync stream.
func (b *LiveDaemonBackend) ApplyPeerUpdates(updates []*pbSync.PeerUpdate, revision uint64, browserURLs map[string]map[int32]string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.generation = revision
	b.peers = make(map[string]*pb.PeerStatus, len(updates))
	for _, u := range updates {
		if !u.GetApproved() || !u.GetOnline() {
			continue
		}
		b.peers[u.GetPeerId()] = &pb.PeerStatus{PeerId: u.GetPeerId(), Alias: u.GetAlias(), AssignedIp: u.GetAssignedIp(), Online: true, ConnectionMode: "unknown", Tags: append([]string(nil), u.GetTags()...), Approved: true, BrowserUrls: maps.Clone(browserURLs[u.GetPeerId()]), ForwardedPorts: append([]int32(nil), u.GetExportedPorts()...)}
	}
	if revision == 0 {
		b.state = "disconnected"
	} else {
		b.state = "running"
	}
	// Snapshot delivery and cancellation share ownership under this lock. A slow
	// subscriber gets the latest snapshot rather than a dropped final revocation.
	for _, ch := range b.subscribers {
		status := b.buildStatusResponseLocked()
		select {
		case ch <- status:
		default:
			select {
			case <-ch:
			default:
			}
			ch <- status
		}
	}
}

func (b *LiveDaemonBackend) SetSyncClient(client *controlsync.Client) {
	b.mu.Lock()
	b.syncClient = client
	b.mu.Unlock()
}
func (b *LiveDaemonBackend) SetApprovalHandler(handler func(context.Context, string, bool) error) {
	b.mu.Lock()
	b.approve = handler
	b.mu.Unlock()
}

func (b *LiveDaemonBackend) buildStatusResponseLocked() *pb.StatusResponse {
	var peerList []*pb.PeerStatus
	for _, p := range b.peers {
		tagsCopy := append([]string(nil), p.Tags...)
		portsCopy := append([]int32(nil), p.ForwardedPorts...)
		peerList = append(peerList, &pb.PeerStatus{
			PeerId:         p.PeerId,
			Alias:          p.Alias,
			AssignedIp:     p.AssignedIp,
			Online:         p.Online,
			LatencyMs:      p.LatencyMs,
			ConnectionMode: p.ConnectionMode,
			Tags:           tagsCopy,
			Approved:       p.Approved,
			ForwardedPorts: portsCopy,
			BrowserUrls:    maps.Clone(p.BrowserUrls),
		})
	}

	return &pb.StatusResponse{
		DaemonState:    b.state,
		DaemonVersion:  b.version,
		Generation:     b.generation,
		ObservedAtUnix: time.Now().Unix(),
		Peers:          peerList,
	}
}

// GetStatus returns the current daemon state and known peers.
func (b *LiveDaemonBackend) GetStatus(ctx context.Context) (*pb.StatusResponse, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.buildStatusResponseLocked(), nil
}

// SubscribeStatus registers a channel for live status updates.
func (b *LiveDaemonBackend) SubscribeStatus(ctx context.Context) (<-chan *pb.StatusResponse, func(), error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	ch := make(chan *pb.StatusResponse, 16)
	b.subscribers = append(b.subscribers, ch)

	cancel := func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		for i, sub := range b.subscribers {
			if sub == ch {
				b.subscribers = append(b.subscribers[:i], b.subscribers[i+1:]...)
				close(ch)
				break
			}
		}
	}

	return ch, cancel, nil
}

// ResolveDevice finds a peer by alias or device ID and returns its dynamic exported ports.
func (b *LiveDaemonBackend) ResolveDevice(ctx context.Context, query string) (*pb.DeviceAddress, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	for _, p := range b.peers {
		if p.PeerId == query || p.Alias == query {
			portsCopy := append([]int32(nil), p.ForwardedPorts...)
			tagsCopy := append([]string(nil), p.Tags...)
			return &pb.DeviceAddress{
				DeviceId:       p.PeerId,
				Alias:          p.Alias,
				AssignedIp:     p.AssignedIp,
				ForwardedPorts: portsCopy,
				BrowserUrls:    maps.Clone(p.BrowserUrls),
				Tags:           tagsCopy,
			}, nil
		}
	}

	return nil, fmt.Errorf("device %q not found in peer table", query)
}

// SetDeviceTags updates device tags via the control sync client.
func (b *LiveDaemonBackend) SetDeviceTags(ctx context.Context, deviceID string, tags []string) error {
	b.mu.RLock()
	client := b.syncClient
	b.mu.RUnlock()
	if client == nil {
		return fmt.Errorf("control sync unavailable; reconnect before changing tags")
	}
	return client.SetTags(ctx, deviceID, tags)
}

// ApprovePeer delegates to the account-root-signed enrollment owner, never the
// topology projection or a locally invented admission flag.
func (b *LiveDaemonBackend) ApprovePeer(ctx context.Context, deviceID string, approved bool) error {
	b.mu.RLock()
	handler := b.approve
	b.mu.RUnlock()
	if handler == nil {
		return fmt.Errorf("signed enrollment approval is unavailable; configure the account signing profile")
	}
	return handler(ctx, deviceID, approved)
}
