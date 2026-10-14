package daemonrpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	pbSync "github.com/pinksaucepasta/paperboat/internal/controlsync/proto"
	pb "github.com/pinksaucepasta/paperboat/internal/daemonrpc/proto"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type mockBackend struct {
	mu          sync.Mutex
	status      *pb.StatusResponse
	subscribers []chan *pb.StatusResponse
	machines    map[string]*pb.MachineAddress
	machineTags map[string][]string
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
		machines: map[string]*pb.MachineAddress{
			"workstation": {
				MachineId:      "dev-1",
				Alias:          "workstation",
				AssignedIp:     "127.100.0.2",
				ForwardedPorts: []int32{22, 8080},
				Tags:           []string{"work", "fast"},
			},
		},
		machineTags: make(map[string][]string),
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

func (m *mockBackend) ResolveMachine(ctx context.Context, query string) (*pb.MachineAddress, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if dev, ok := m.machines[query]; ok {
		tags := m.machineTags[dev.MachineId]
		if len(tags) > 0 {
			dev.Tags = tags
		}
		return dev, nil
	}
	return nil, fmt.Errorf("machine %q not found", query)
}

func (m *mockBackend) ApprovePeer(ctx context.Context, machineID string, approved bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return nil
}

func (m *mockBackend) SetMachineTags(ctx context.Context, machineID string, tags []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.machineTags[machineID] = tags
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

	// 1. Test ResolveMachine
	dev, err := client.ResolveMachine(ctx, "workstation")
	if err != nil {
		t.Fatalf("ResolveMachine() error: %v", err)
	}
	if dev.MachineId != "dev-1" || dev.AssignedIp != "127.100.0.2" {
		t.Fatalf("unexpected resolved machine: %+v", dev)
	}

	// 2. Test SetMachineTags and then resolve again
	tagResp, err := client.SetMachineTags(ctx, "dev-1", []string{"production", "gateway"})
	if err != nil {
		t.Fatalf("SetMachineTags() error: %v", err)
	}
	if !tagResp.Success || !reflect.DeepEqual(tagResp.Tags, []string{"production", "gateway"}) {
		t.Fatalf("unexpected SetMachineTags response: %+v", tagResp)
	}

	dev, err = client.ResolveMachine(ctx, "workstation")
	if err != nil {
		t.Fatalf("ResolveMachine after tags error: %v", err)
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

type streamRaceBackend struct {
	mu          sync.Mutex
	current     *pb.StatusResponse
	subscribers []chan *pb.StatusResponse
	getEntered  chan struct{}
	releaseGet  chan struct{}
	getOnce     sync.Once
}

func (b *streamRaceBackend) GetStatus(ctx context.Context) (*pb.StatusResponse, error) {
	b.mu.Lock()
	current := b.current
	b.mu.Unlock()
	b.getOnce.Do(func() { close(b.getEntered) })
	select {
	case <-b.releaseGet:
		return current, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (b *streamRaceBackend) SubscribeStatus(context.Context) (<-chan *pb.StatusResponse, func(), error) {
	updates := make(chan *pb.StatusResponse, 8)
	b.mu.Lock()
	b.subscribers = append(b.subscribers, updates)
	b.mu.Unlock()
	var once sync.Once
	return updates, func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			for index, subscriber := range b.subscribers {
				if subscriber == updates {
					b.subscribers = append(b.subscribers[:index], b.subscribers[index+1:]...)
					close(updates)
					return
				}
			}
		})
	}, nil
}

func (*streamRaceBackend) ResolveMachine(context.Context, string) (*pb.MachineAddress, error) {
	return nil, nil
}
func (*streamRaceBackend) SetMachineTags(context.Context, string, []string) error { return nil }
func (*streamRaceBackend) ApprovePeer(context.Context, string, bool) error        { return nil }

func (b *streamRaceBackend) apply(status *pb.StatusResponse) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.current = status
	for _, subscriber := range b.subscribers {
		select {
		case subscriber <- status:
		default:
			select {
			case <-subscriber:
			default:
			}
			subscriber <- status
		}
	}
}

func TestStreamStatusDoesNotMissUpdateDuringInitialSnapshot(t *testing.T) {
	backend := &streamRaceBackend{
		current:    &pb.StatusResponse{DaemonState: "connected", Generation: 1},
		getEntered: make(chan struct{}),
		releaseGet: make(chan struct{}),
	}
	socket := testSocket(t)
	listener, err := Listen(socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server := NewServer(backend)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
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
	select {
	case <-backend.getEntered:
	case <-ctx.Done():
		t.Fatal("initial status read did not begin")
	}
	backend.apply(&pb.StatusResponse{DaemonState: "disconnected", Generation: 2})
	close(backend.releaseGet)
	initial, err := stream.Recv()
	if err != nil || initial.Generation != 1 {
		t.Fatalf("initial status=%#v err=%v", initial, err)
	}
	update, err := stream.Recv()
	if err != nil || update.Generation != 2 || update.DaemonState != "disconnected" {
		t.Fatalf("update=%#v err=%v", update, err)
	}
}

type rpcBoundaryBackend struct {
	mu         sync.Mutex
	resolveErr error
	address    *pb.MachineAddress
}

func TestResolveMachineDirectFailureRetainsCauseSafely(t *testing.T) {
	cause := errors.Join(ErrMachineNotFound, fmt.Errorf("private endpoint /var/run/daemon-secret.sock: %w", syscall.EIO))
	backend := &rpcBoundaryBackend{resolveErr: cause}
	ctx := supportref.WithContext(context.Background(), "support_01234567-89ab-4def-8123-456789abcdef")
	_, err := NewServer(backend).ResolveMachine(ctx, &pb.MachineQuery{Query: "private-query"})
	if !errors.Is(err, syscall.EIO) {
		t.Fatal("server failure did not preserve its original cause")
	}
	if strings.Contains(err.Error(), "/var/run/daemon-secret.sock") || strings.Contains(err.Error(), "private-query") {
		t.Fatalf("server error exposed backend details: %q", err)
	}
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("server error code=%s, want Unavailable", status.Code(err))
	}
}

func (b *rpcBoundaryBackend) GetStatus(context.Context) (*pb.StatusResponse, error) {
	return &pb.StatusResponse{DaemonState: "running"}, nil
}

func (*rpcBoundaryBackend) SubscribeStatus(context.Context) (<-chan *pb.StatusResponse, func(), error) {
	return make(chan *pb.StatusResponse), func() {}, nil
}

func (b *rpcBoundaryBackend) ResolveMachine(context.Context, string) (*pb.MachineAddress, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.address, b.resolveErr
}

func (*rpcBoundaryBackend) SetMachineTags(context.Context, string, []string) error { return nil }
func (*rpcBoundaryBackend) ApprovePeer(context.Context, string, bool) error        { return nil }

func (b *rpcBoundaryBackend) setResolve(err error, address *pb.MachineAddress) {
	b.mu.Lock()
	b.resolveErr, b.address = err, address
	b.mu.Unlock()
}

type statusSendTestStream struct {
	grpc.ServerStream
	ctx     context.Context
	sendErr error
}

func (s *statusSendTestStream) Context() context.Context { return s.ctx }
func (s *statusSendTestStream) Send(*pb.StatusResponse) error {
	return s.sendErr
}

func TestStreamStatusSafeFailureOwner(t *testing.T) {
	reference := "support_01234567-89ab-4def-8123-456789abcdef"
	tests := []struct {
		name      string
		sendErr   error
		wantCode  codes.Code
		wantFault int
	}{
		{name: "gRPC canceled", sendErr: status.Error(codes.Canceled, "client closed"), wantCode: codes.Canceled},
		{name: "gRPC deadline", sendErr: status.Error(codes.DeadlineExceeded, "client deadline"), wantCode: codes.DeadlineExceeded},
		{name: "EOF", sendErr: io.EOF, wantCode: codes.Canceled},
		{name: "mixed cancellation and storage failure", sendErr: errors.Join(status.Error(codes.Canceled, "client closed"), fmt.Errorf("private file /srv/secret/key: %w", syscall.EIO)), wantCode: codes.Unavailable, wantFault: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var faults []errorreport.Fault
			restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) { faults = append(faults, fault) })
			defer restore()
			ctx := supportref.WithContext(context.Background(), reference)
			stream := &statusSendTestStream{ctx: ctx, sendErr: test.sendErr}
			err := NewServer(newMockBackend()).StreamStatus(&pb.Empty{}, stream)
			if status.Code(err) != test.wantCode {
				t.Fatalf("stream error code=%s want=%s err=%v", status.Code(err), test.wantCode, err)
			}
			if strings.Contains(err.Error(), "/srv/secret/key") {
				t.Fatalf("stream error exposed cause: %q", err)
			}
			if got := len(faults); got != test.wantFault {
				t.Fatalf("observed %d stream faults, want %d", got, test.wantFault)
			}
			if test.wantFault == 1 && (!errors.Is(err, syscall.EIO) || faults[0].Errno != int(syscall.EIO) || faults[0].SupportReference != reference) {
				t.Fatalf("stream error lost cause or correlation: error=%v fault=%+v", err, faults[0])
			}
		})
	}
}

func TestStreamStatusUsesContextCancelCause(t *testing.T) {
	reference := "support_01234567-89ab-4def-8123-456789abcdef"
	ctx, cancel := context.WithCancelCause(supportref.WithContext(context.Background(), reference))
	cancel(fmt.Errorf("parent cleanup at /srv/private: %w", syscall.EIO))
	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) { faults = append(faults, fault) })
	defer restore()
	stream := &statusSendTestStream{ctx: ctx}
	err := NewServer(newMockBackend()).StreamStatus(&pb.Empty{}, stream)
	if status.Code(err) != codes.Unavailable || !errors.Is(err, syscall.EIO) || strings.Contains(err.Error(), "/srv/private") {
		t.Fatalf("parent cancellation cause was not safely preserved: code=%s err=%v", status.Code(err), err)
	}
	if len(faults) != 1 || faults[0].Errno != int(syscall.EIO) || faults[0].SupportReference != reference {
		t.Fatalf("parent cancellation cause was not reported once with its reference: %+v", faults)
	}
}

func TestResolveMachineProjectsBackendFailuresSafelyAndRecovers(t *testing.T) {
	backend := &rpcBoundaryBackend{}
	listener := bufconn.Listen(1 << 20)
	server := NewServer(backend)
	go func() { _ = server.grpcServer.Serve(listener) }()
	defer server.Stop()
	defer listener.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(ctx, "bufnet",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
		grpc.WithBlock(),
	)
	if err != nil {
		t.Fatalf("dial bufconn: %v", err)
	}
	defer conn.Close()
	client := pb.NewDaemonServiceClient(conn)

	var mu sync.Mutex
	var faults []errorreport.Fault
	restore := errorreport.InstallFaultObserver(func(_ context.Context, fault errorreport.Fault) {
		mu.Lock()
		faults = append(faults, fault)
		mu.Unlock()
	})
	defer restore()

	backend.setResolve(errors.Join(
		fmt.Errorf("%w: hidden-machine-query", ErrMachineNotFound),
		fmt.Errorf("private-key=/srv/secret/key: %w", syscall.EIO),
	), nil)
	_, err = client.ResolveMachine(rpcContext(ctx), &pb.MachineQuery{Query: "hidden-machine-query"})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("mixed lookup error code=%s err=%v", status.Code(err), err)
	}
	message := status.Convert(err).Message()
	if strings.Contains(message, "hidden-machine-query") || strings.Contains(message, "/srv/secret/key") || strings.Contains(message, "input/output error") {
		t.Fatalf("gRPC status exposed backend details: %q", message)
	}
	marker := "support reference: "
	_, reference, ok := strings.Cut(message, marker)
	reference = strings.TrimSuffix(reference, ")")
	if !ok || !supportref.Valid(reference) {
		t.Fatalf("gRPC status lacks safe support reference: %q", message)
	}
	mu.Lock()
	if len(faults) != 1 {
		mu.Unlock()
		t.Fatalf("observed %d faults, want one", len(faults))
	}
	fault := faults[0]
	mu.Unlock()
	if fault.SupportReference != reference || fault.Errno != int(syscall.EIO) || fault.Code != "control_request_failed" || fault.Stage != "control_request" || fault.Outcome != "failed" {
		t.Fatalf("fault lost bounded cause or correlation: %+v", fault)
	}

	backend.setResolve(context.Canceled, nil)
	_, err = client.ResolveMachine(rpcContext(ctx), &pb.MachineQuery{Query: "workstation"})
	if status.Code(err) != codes.Canceled {
		t.Fatalf("canceled lookup code=%s err=%v", status.Code(err), err)
	}
	mu.Lock()
	observed := len(faults)
	mu.Unlock()
	if observed != 1 {
		t.Fatalf("ordinary cancellation added a fault; count=%d", observed)
	}

	backend.setResolve(fmt.Errorf("%w: hidden-machine-query", ErrMachineNotFound), nil)
	_, err = client.ResolveMachine(rpcContext(ctx), &pb.MachineQuery{Query: "hidden-machine-query"})
	if status.Code(err) != codes.NotFound || strings.Contains(status.Convert(err).Message(), "hidden-machine-query") {
		t.Fatalf("expected safe not-found status, got code=%s message=%q", status.Code(err), status.Convert(err).Message())
	}
	mu.Lock()
	observed = len(faults)
	mu.Unlock()
	if observed != 1 {
		t.Fatalf("expected not-found added a fault; count=%d", observed)
	}

	backend.setResolve(nil, &pb.MachineAddress{MachineId: "machine-123", AssignedIp: "127.100.0.2"})
	resolved, err := client.ResolveMachine(rpcContext(ctx), &pb.MachineQuery{Query: "workstation"})
	if err != nil || resolved.GetMachineId() != "machine-123" {
		t.Fatalf("lookup did not recover: response=%+v err=%v", resolved, err)
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

	// 1. Resolve machine and verify exported ports are dynamic, not static
	addr, err := backend.ResolveMachine(context.Background(), "dev-box")
	if err != nil {
		t.Fatalf("ResolveMachine failed: %v", err)
	}
	if addr.MachineId != "node-xyz" || addr.AssignedIp != "127.100.0.5" {
		t.Fatalf("unexpected machine address: %+v", addr)
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
