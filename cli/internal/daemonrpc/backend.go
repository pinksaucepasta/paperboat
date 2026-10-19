package daemonrpc

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/controlsync"
	pbSync "github.com/pinksaucepasta/paperboat/internal/controlsync/proto"
	pb "github.com/pinksaucepasta/paperboat/internal/daemonrpc/proto"
)

// ErrMachineNotFound identifies the only expected negative result from a
// machine lookup. Callers can distinguish it from failures that happen while
// consulting the peer table without inspecting error text.
var ErrMachineNotFound = errors.New("machine not found")

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
	accountID   string
	accountName string
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
		b.peers[u.GetPeerId()] = &pb.PeerStatus{PeerId: u.GetPeerId(), Alias: u.GetAlias(), AssignedIp: u.GetAssignedIp(), Online: true, ConnectionMode: "unknown", Approved: true, BrowserUrls: maps.Clone(browserURLs[u.GetPeerId()]), ForwardedPorts: append([]int32(nil), u.GetExportedPorts()...)}
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
// SetAccount records the signed-in account this daemon serves.
func (b *LiveDaemonBackend) SetAccount(id, label string) {
	b.mu.Lock()
	b.accountID, b.accountName = id, label
	b.mu.Unlock()
}

// Account returns the signed-in account this daemon serves.
func (b *LiveDaemonBackend) Account() (string, string) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.accountID, b.accountName
}

func (b *LiveDaemonBackend) SetApprovalHandler(handler func(context.Context, string, bool) error) {
	b.mu.Lock()
	b.approve = handler
	b.mu.Unlock()
}

func (b *LiveDaemonBackend) buildStatusResponseLocked() *pb.StatusResponse {
	var peerList []*pb.PeerStatus
	for _, p := range b.peers {
		portsCopy := append([]int32(nil), p.ForwardedPorts...)
		peerList = append(peerList, &pb.PeerStatus{
			PeerId:         p.PeerId,
			Alias:          p.Alias,
			AssignedIp:     p.AssignedIp,
			Online:         p.Online,
			LatencyMs:      p.LatencyMs,
			ConnectionMode: p.ConnectionMode,
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

// ResolveMachine finds a peer by alias or machine ID and returns its dynamic exported ports.
func (b *LiveDaemonBackend) ResolveMachine(ctx context.Context, query string) (*pb.MachineAddress, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	for _, p := range b.peers {
		if p.PeerId == query || p.Alias == query {
			portsCopy := append([]int32(nil), p.ForwardedPorts...)
			return &pb.MachineAddress{
				MachineId:      p.PeerId,
				Alias:          p.Alias,
				AssignedIp:     p.AssignedIp,
				ForwardedPorts: portsCopy,
				BrowserUrls:    maps.Clone(p.BrowserUrls),
			}, nil
		}
	}

	return nil, fmt.Errorf("%w: %q", ErrMachineNotFound, query)
}

// ApprovePeer delegates to the account-root-signed enrollment owner, never the
// topology projection or a locally invented admission flag.
func (b *LiveDaemonBackend) ApprovePeer(ctx context.Context, machineID string, approved bool) error {
	b.mu.RLock()
	handler := b.approve
	b.mu.RUnlock()
	if handler == nil {
		return fmt.Errorf("signed enrollment approval is unavailable; configure the account signing profile")
	}
	return handler(ctx, machineID, approved)
}
