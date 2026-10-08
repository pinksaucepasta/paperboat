package edgehttp

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/control"
	"github.com/pinksaucepasta/paperboat-tunnel/internal/usage"
	"io"
	"net"
	"sync"
	"testing"
	"time"

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

func testIngressLimits(connections int, byteRate rate.Limit) *IngressLimits {
	return &IngressLimits{Publication: IngressLimitConfig{Connections: connections, OpenRate: 100, OpenBurst: 100, ByteRate: byteRate, ByteBurst: ingressPacingChunk}, Account: IngressLimitConfig{Connections: connections, OpenRate: 100, OpenBurst: 100, ByteRate: byteRate, ByteBurst: ingressPacingChunk}}
}

func TestIngressLimitsSharePublicationAndAccountAcrossCarriers(t *testing.T) {
	registry, err := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 2, IngressLimits: testIngressLimits(1, 1<<20)})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	first := publicTCPDecision(time.Now().UTC(), 4001)
	lease, err := registry.AcquireIngress(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	second := first
	second.DecisionID, second.ConnectorID, second.SessionID = "decision_other", "connector_other", "session_other"
	if _, err := registry.AcquireIngress(context.Background(), second); !errors.Is(err, ErrIngressLimitExceeded) {
		t.Fatalf("shared publication limit error=%v", err)
	}
	lease.Release()
	if next, err := registry.AcquireIngress(context.Background(), second); err != nil {
		t.Fatal(err)
	} else {
		next.Release()
	}
}

func TestIngressOpenRateIsSharedAfterConnectionsClose(t *testing.T) {
	limits := testIngressLimits(4, 1<<20)
	limits.Publication.OpenRate, limits.Publication.OpenBurst = rate.Limit(0.01), 1
	limits.Account.OpenRate, limits.Account.OpenBurst = rate.Limit(0.01), 1
	registry, err := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 2, IngressLimits: limits})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	decision := publicTCPDecision(time.Now().UTC(), 4001)
	first, err := registry.AcquireIngress(context.Background(), decision)
	if err != nil {
		t.Fatal(err)
	}
	first.Release()
	decision.ConnectorID, decision.SessionID = "connector_other", "session_other"
	if _, err := registry.AcquireIngress(context.Background(), decision); !errors.Is(err, ErrIngressLimitExceeded) {
		t.Fatalf("shared open rate error=%v", err)
	}
}

func TestIngressPacingCancellationMetersSuccessfulPartialBytesAndReleases(t *testing.T) {
	recorder := &ingressUsageRecorder{}
	registry, err := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 2, IngressLimits: testIngressLimits(1, 1), Usage: recorder})
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
	registry, err := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 2, IngressLimits: testIngressLimits(1, 1<<20), Usage: recorder})
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

func TestIngressLifetimeClosesBlockedStreamAndReleasesCapacity(t *testing.T) {
	registry, err := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 2, IngressLimits: testIngressLimits(1, 1<<20)})
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
	stream := lease.Wrap(context.Background(), local)
	defer stream.Close()
	done := make(chan error, 1)
	go func() { _, err := stream.Read(make([]byte, 1)); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expired read succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("lifetime did not unblock stream")
	}
	// Close synchronizes the deadline callback's release before checking capacity.
	_ = stream.Close()
	next, err := registry.AcquireIngress(context.Background(), decision)
	if err != nil {
		t.Fatalf("expired stream retained capacity: %v", err)
	}
	next.Release()
}

func (r *ingressUsageRecorder) SubscribeQuota(string, time.Time, func()) func() { return func() {} }

type quotaFeedbackSink struct{}

func (quotaFeedbackSink) ReportUsage(context.Context, control.UsageReport) (control.UsageResult, error) {
	return control.UsageResult{Disposition: "accounted", QuotaExhausted: true}, nil
}

func TestIngressQuotaFeedbackIsolatesSharedRouteAndAllowsFreshAdmission(t *testing.T) {
	_, private, _ := ed25519.GenerateKey(nil)
	now := time.Now().UTC()
	q, _ := usage.NewQueue(4, 1<<20)
	meter := &usage.Meter{Node: "edge", Epoch: "epoch", Counters: usage.NewCounters(), Queue: q, KeyID: "key", PrivateKey: private, Persist: func() error { return nil }}
	registry, err := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 2, IngressLimits: testIngressLimits(4, 1<<20), Usage: meter})
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	open := func(grant string, at time.Time) (io.ReadWriteCloser, net.Conn) {
		t.Helper()
		_, d := lazyRuleAndDecision("quota.example.test")
		// These validated private decisions share the same route but carry distinct grants.
		d.Binding.Audience = "team"
		d.PrincipalID = "member"
		d.GrantID = grant
		d.GrantGeneration = 1
		d.MembershipGeneration = 1
		lease, e := registry.AcquireIngress(context.Background(), d)
		if e != nil {
			t.Fatal(e)
		}
		lease.decision.IssuedAt = at
		local, remote := net.Pipe()
		wrapped := lease.Wrap(context.Background(), local)
		t.Cleanup(func() { _ = wrapped.Close(); _ = remote.Close() })
		return wrapped, remote
	}
	a, _ := open("team-a", now.Add(-time.Second))
	b, bPeer := open("team-b", now.Add(-time.Second))
	done := make(chan error, 1)
	go func() { _, e := a.Read(make([]byte, 1)); done <- e }()
	report, e := usage.NewSignedReport("key", private, usage.SignedDocument{OperationID: "quota_report", Key: usage.Key{Authority: "grant:team-a", Node: "edge", Epoch: "epoch", Environment: "env", Route: "route", Revision: 1, Direction: "ingress"}, Bytes: 100, Start: now, End: now})
	if e != nil {
		t.Fatal(e)
	}
	if e = q.Enqueue(report); e != nil {
		t.Fatal(e)
	}
	if _, _, e = control.DeliverNext(context.Background(), q, quotaFeedbackSink{}); e != nil {
		t.Fatal(e)
	}
	select {
	case e := <-done:
		if !errors.Is(e, ErrIngressQuotaExhausted) {
			t.Fatalf("quota closure error=%v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("exhaustion did not close active stream")
	}
	// Another team's grant remains usable on the same route.
	writeDone := make(chan error, 1)
	go func() { _, e := bPeer.Write([]byte("b")); writeDone <- e }()
	buf := make([]byte, 1)
	if _, e = b.Read(buf); e != nil || buf[0] != 'b' {
		t.Fatalf("other workspace disrupted: %v", e)
	}
	if e = <-writeDone; e != nil {
		t.Fatal(e)
	}
	// A newly authorized stream after period recovery ignores delayed old feedback.
	fresh, freshPeer := open("team-a", now.Add(time.Second))
	q.ExhaustAuthority("grant:team-a", now)
	go func() { _, e := freshPeer.Write([]byte("f")); writeDone <- e }()
	if _, e = fresh.Read(buf); e != nil || buf[0] != 'f' {
		t.Fatalf("fresh period admission closed by old receipt: %v", e)
	}
	if e = <-writeDone; e != nil {
		t.Fatal(e)
	}
	_ = fresh.Close()
	_ = b.Close()
	_ = a.Close()
}
