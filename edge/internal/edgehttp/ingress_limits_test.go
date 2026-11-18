package edgehttp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/edge/internal/usage"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/connectorprotocol"
	"github.com/pinksaucepasta/paperboat/shared/edgelimits"
	"golang.org/x/time/rate"
)

type ingressUsageRecord struct {
	environment, route        string
	revision, ingress, egress uint64
}
type ingressUsageRecorder struct {
	mu      sync.Mutex
	records []ingressUsageRecord
}

func (r *ingressUsageRecorder) Record(environment, route string, revision uint64, ingress, egress uint64, authorityID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, ingressUsageRecord{environment, route, revision, ingress, egress})
	return nil
}

type ingressMemoryStream struct {
	bytes.Buffer
	writeLimit int
	writeErr   error
	closed     bool
}

func (s *ingressMemoryStream) Read(payload []byte) (int, error) { return s.Buffer.Read(payload) }
func (s *ingressMemoryStream) Write(payload []byte) (int, error) {
	if s.writeLimit > 0 && len(payload) > s.writeLimit {
		n, _ := s.Buffer.Write(payload[:s.writeLimit])
		return n, s.writeErr
	}
	return s.Buffer.Write(payload)
}
func (s *ingressMemoryStream) Close() error      { s.closed = true; return nil }
func (s *ingressMemoryStream) CloseWrite() error { return nil }

func testIngressLimits(byteRate rate.Limit) *IngressLimits {
	return &IngressLimits{Publication: IngressLimitConfig{ByteRate: byteRate, ByteBurst: ingressPacingChunk}, Account: IngressLimitConfig{ByteRate: byteRate, ByteBurst: ingressPacingChunk}, Throttled: IngressLimitConfig{ByteRate: byteRate, ByteBurst: ingressPacingChunk}}
}

// Section 5 of the edge abuse plan: popularity is not abuse. Any number of concurrent streams and
// opens of one publication are admitted; they share one byte pacer per publication and account.
func TestIngressNeverRefusesStreamsAndSharesPacers(t *testing.T) {
	registry, err := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 2, IngressLimits: testIngressLimits(1 << 20)})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	decision := publicTCPDecision(time.Now().UTC(), 4001)
	var leases []*IngressLease
	for i := 0; i < 2000; i++ {
		lease, err := registry.AcquireIngress(context.Background(), decision)
		if err != nil {
			t.Fatalf("stream %d refused: %v", i, err)
		}
		leases = append(leases, lease)
	}
	if leases[0].publication != leases[1999].publication || leases[0].account != leases[1999].account {
		t.Fatal("streams of one publication do not share its pacers")
	}
	if got := leases[0].publication.active; got != 2000 {
		t.Fatalf("active=%d", got)
	}
	for _, lease := range leases {
		lease.Release()
	}
	if got := leases[0].publication.active; got != 0 {
		t.Fatalf("active after release=%d", got)
	}
	expired := decision
	expired.ExpiresAt = time.Now().Add(-time.Minute)
	if _, err := registry.AcquireIngress(context.Background(), expired); !errors.Is(err, ErrIngressDecisionInvalid) {
		t.Fatalf("expired decision error=%v", err)
	}
}

// A full table of keys with open streams gives a new key its own pacer, never a refusal.
func TestIngressFullTableStillAdmits(t *testing.T) {
	limits, err := newIngressLimits(testIngressLimits(1 << 20))
	if err != nil {
		t.Fatal(err)
	}
	decision := publicTCPDecision(time.Now().UTC(), 4001)
	for i := 0; i < maximumIngressLimitEntries; i++ {
		decision.Binding.AccountID = fmt.Sprintf("account_%d", i)
		limits.acquire(decision, nil)
	}
	if n := len(limits.publications) + len(limits.accounts); n != maximumIngressLimitEntries {
		t.Fatalf("table size %d", n)
	}
	decision.Binding.AccountID = "account_new"
	if lease := limits.acquire(decision, nil); lease == nil || lease.publication == nil {
		t.Fatal("new key not admitted on a full table")
	}
	if n := len(limits.publications) + len(limits.accounts); n != maximumIngressLimitEntries {
		t.Fatalf("table grew past its bound: %d", n)
	}
}

func TestIngressPacingCancellationMetersSuccessfulPartialBytesAndReleases(t *testing.T) {
	recorder := &ingressUsageRecorder{}
	registry, err := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 2, IngressLimits: testIngressLimits(1), Usage: recorder})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	decision := publicTCPDecision(time.Now().UTC(), 4001)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	lease, err := registry.AcquireIngress(ctx, decision)
	if err != nil {
		t.Fatal(err)
	}
	underlying := &ingressMemoryStream{}
	stream := lease.Wrap(ctx, underlying)
	written, err := stream.Write(make([]byte, ingressPacingChunk*2))
	if err == nil || written != ingressPacingChunk {
		t.Fatalf("paced write=%d err=%v", written, err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if !underlying.closed {
		t.Fatal("paced stream was not closed")
	}
	if len(recorder.records) != 1 || recorder.records[0].ingress != ingressPacingChunk || recorder.records[0].egress != 0 {
		t.Fatalf("usage=%+v", recorder.records)
	}
	if next, err := registry.AcquireIngress(context.Background(), decision); err != nil {
		t.Fatalf("connection permit leaked: %v", err)
	} else {
		next.Release()
	}
}

func TestIngressMeterRecordsBytesReturnedWithError(t *testing.T) {
	recorder := &ingressUsageRecorder{}
	registry, err := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 2, IngressLimits: testIngressLimits(1 << 20), Usage: recorder})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	decision := publicTCPDecision(time.Now().UTC(), 4001)
	lease, err := registry.AcquireIngress(context.Background(), decision)
	if err != nil {
		t.Fatal(err)
	}
	stream := lease.Wrap(context.Background(), &ingressMemoryStream{writeLimit: 7, writeErr: io.ErrUnexpectedEOF})
	n, err := stream.Write([]byte("partial payload"))
	if n != 7 || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("partial write=%d err=%v", n, err)
	}
	_ = stream.Close()
	if len(recorder.records) != 1 || recorder.records[0] != (ingressUsageRecord{environment: decision.Binding.EnvironmentID, route: decision.Binding.RouteID, revision: decision.Binding.RouteGeneration, ingress: 7}) {
		t.Fatalf("usage=%+v", recorder.records)
	}
}

var _ io.ReadWriteCloser = (*ingressMemoryStream)(nil)
var _ interface{ CloseWrite() error } = (*ingressMemoryStream)(nil)
var _ IngressUsage = (*ingressUsageRecorder)(nil)

func TestIngressOutlivesAuthorityWindowUntilDisconnectedAndReleasesCapacity(t *testing.T) {
	registry, err := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 2, IngressLimits: testIngressLimits(1 << 20)})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	decision := publicTCPDecision(time.Now().UTC(), 4001)
	lease, err := registry.AcquireIngress(context.Background(), decision)
	if err != nil {
		t.Fatal(err)
	}
	lease.decision.IssuedAt = time.Now().Add(-usage.DeliveryWindow + 20*time.Millisecond)
	local, remote := net.Pipe()
	defer remote.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := lease.Wrap(ctx, local)
	defer stream.Close()
	done := make(chan error, 1)
	go func() { _, err := stream.Read(make([]byte, 1)); done <- err }()
	select {
	case <-done:
		t.Fatal("credential delivery window ended admitted stream")
	case <-time.After(60 * time.Millisecond):
	}
	go func() { _, _ = remote.Write([]byte{1}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("admitted payload blocked")
	}
	go func() { _, err := stream.Read(make([]byte, 1)); done <- err }()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("disconnect did not interrupt read")
		}
	case <-time.After(time.Second):
		t.Fatal("disconnect retained stream")
	}

	// Close synchronizes the deadline callback's release before checking capacity.
	_ = stream.Close()
	next, err := registry.AcquireIngress(context.Background(), decision)
	if err != nil {
		t.Fatalf("expired stream retained capacity: %v", err)
	}
	next.Release()
}

// A workspace the server lists as throttled has all its routes paced by one shared bucket at the throttled
// rate (its streams stay open and the bytes pass), another workspace runs at full speed, and the pace ends at
// the reset time or when the server stops listing the workspace.
func TestThrottledWorkspaceIsPacedAcrossItsRoutesAndRecovers(t *testing.T) {
	limits := &IngressLimits{Publication: IngressLimitConfig{ByteRate: 1 << 30, ByteBurst: 1 << 20}, Account: IngressLimitConfig{ByteRate: 1 << 30, ByteBurst: 1 << 20}, Throttled: IngressLimitConfig{ByteRate: 64 << 10, ByteBurst: ingressPacingChunk}}
	limits, err := newIngressLimits(limits)
	if err != nil {
		t.Fatal(err)
	}
	throttles := limits.Throttles
	throttles.Set([]connectorprotocol.IngressThrottle{
		{Hostname: "a.example.test", Workspace: "personal:one"},
		{Hostname: "b.example.test", Workspace: "personal:one"},
		{Hostname: "c.example.test", Workspace: "team:two", ResetsAt: time.Now().Add(-time.Second)},
	})
	ctx := context.Background()
	// Both routes of one workspace share one bucket: the burst is consumed by the first, so the second waits.
	if err := throttles.Wait(ctx, "a.example.test", ingressPacingChunk); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := throttles.Wait(ctx, "b.example.test", ingressPacingChunk); err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(started); waited < 300*time.Millisecond {
		t.Fatalf("a second route of the throttled workspace waited %v, want about 0.5 s at 64 KiB/s", waited)
	}
	// A reset time in the past and an unlisted route are not paced.
	started = time.Now()
	for _, host := range []string{"c.example.test", "unlisted.example.test"} {
		if err := throttles.Wait(ctx, host, 4*ingressPacingChunk); err != nil {
			t.Fatal(err)
		}
	}
	if waited := time.Since(started); waited > 100*time.Millisecond {
		t.Fatalf("routes at full speed waited %v", waited)
	}
	// The waiting honors cancellation instead of holding the stream forever.
	short, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := throttles.Wait(short, "a.example.test", 8*ingressPacingChunk); err == nil {
		t.Fatal("a paced wait outlived its context")
	}
	throttles.Set(nil)
	started = time.Now()
	if err := throttles.Wait(ctx, "a.example.test", 8*ingressPacingChunk); err != nil || time.Since(started) > 100*time.Millisecond {
		t.Fatalf("workspace stayed paced after the server stopped listing it: %v", err)
	}
}

type failingIngressUsage struct{ ingressUsageRecorder }

func (*failingIngressUsage) Record(string, string, uint64, uint64, uint64, string) error {
	return syscall.EIO
}

type failingIngressClose struct{ net.Conn }

func (s failingIngressClose) Close() error { return errors.Join(s.Conn.Close(), syscall.EIO) }

func TestIngressRetainsTransferAndCleanupCauses(t *testing.T) {
	registry, err := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 2, IngressLimits: testIngressLimits(1 << 20), Usage: &failingIngressUsage{}})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	decision := publicTCPDecision(time.Now().UTC(), 4001)
	lease, err := registry.AcquireIngress(context.Background(), decision)
	if err != nil {
		t.Fatal(err)
	}
	stream := lease.Wrap(context.Background(), &ingressMemoryStream{writeLimit: 1, writeErr: io.ErrUnexpectedEOF})
	if n, err := stream.Write([]byte("ab")); n != 1 || !errors.Is(err, syscall.EIO) || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("lost partial transfer causes: n=%d err=%v", n, err)
	}
	_ = stream.Close()
	lease, err = registry.AcquireIngress(context.Background(), decision)
	if err != nil {
		t.Fatal(err)
	}
	local, peer := net.Pipe()
	defer peer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	wrapped := lease.Wrap(ctx, failingIngressClose{local})
	cancel()
	if err := wrapped.Close(); !errors.Is(err, syscall.EIO) {
		t.Fatalf("lost cleanup cause: %v", err)
	}
	if err := wrapped.Close(); !errors.Is(err, syscall.EIO) {
		t.Fatal("repeated cleanup lost first cause")
	}
	next, err := registry.AcquireIngress(context.Background(), decision)
	if err != nil {
		t.Fatalf("cleanup retained capacity: %v", err)
	}
	next.Release()
}

// Audit T11: the server's configured set replaces the defaults on new and current keys alike,
// and an invalid set changes nothing.
func TestServerIngressLimitsApplyToCurrentKeys(t *testing.T) {
	registry, err := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	decision := publicTCPDecision(time.Now().UTC(), 4100)
	first, err := registry.AcquireIngress(context.Background(), decision)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	set := edgelimits.Default()
	set.Publication.BytesPerSecond = 1 << 20
	if err := registry.SetIngressLimits(set); err != nil {
		t.Fatal(err)
	}
	bad := set
	bad.Account.ByteBurst = 0
	if err := registry.SetIngressLimits(bad); err == nil {
		t.Fatal("invalid set applied")
	}
	if registry.ingressLimits.Publication.ByteRate != 1<<20 || registry.ingressLimits.Account != ingressLimitConfig(set.Account) {
		t.Fatalf("limits after an invalid set: %+v", registry.ingressLimits.Publication)
	}
	state := registry.ingressLimits.publications[decision.Binding.AccountID+"\x00"+decision.Binding.PublicationID]
	if state == nil || state.bytes.Limit() != rate.Limit(set.Publication.BytesPerSecond) {
		t.Fatalf("current key not reconfigured: %+v", state)
	}
}

// F21-47: an edge that has not heard from the server (self-hosted without an answer yet) enforces
// the shared defaults, the same set the server sends.
func TestEdgeWithoutAnswerUsesTheSharedDefaultLimits(t *testing.T) {
	registry, err := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	set := edgelimits.Default()
	if registry.ingressLimits.Publication != ingressLimitConfig(set.Publication) || registry.ingressLimits.Account != ingressLimitConfig(set.Account) {
		t.Fatalf("edge limits = %+v / %+v", registry.ingressLimits.Publication, registry.ingressLimits.Account)
	}
}

// An operator reads back what an edge applies from one log line, including the first answer, which equals
// the defaults and used to change nothing silently (F21-47).
func TestEdgeLogsTheIngressLimitsItApplies(t *testing.T) {
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, nil)))
	defer slog.SetDefault(previous)
	registry, err := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	set := edgelimits.Default()
	for range 2 { // the same answer again is not logged twice
		if err := registry.SetIngressLimits(set); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Count(logged.String(), "ingress_limits_applied"); got != 1 || !strings.Contains(logged.String(), "account_bytes_per_second=3.3554432e+07") {
		t.Fatalf("log = %q", logged.String())
	}
	set.Publication.BytesPerSecond /= 2
	if err := registry.SetIngressLimits(set); err != nil || strings.Count(logged.String(), "ingress_limits_applied") != 2 {
		t.Fatalf("a changed set must be logged: err=%v log=%q", err, logged.String())
	}
}
