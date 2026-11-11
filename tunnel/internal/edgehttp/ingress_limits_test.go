package edgehttp

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/control"
	"github.com/pinksaucepasta/paperboat/tunnel/internal/usage"
	"io"
	"net"
	"sync"
	"syscall"
	"testing"
	"time"

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
	return &IngressLimits{Publication: IngressLimitConfig{ByteRate: byteRate, ByteBurst: ingressPacingChunk}, Account: IngressLimitConfig{ByteRate: byteRate, ByteBurst: ingressPacingChunk}}
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
	registry, err := NewDataCarrierRouteRegistry(DataCarrierRouteRegistryConfig{MaximumRoutes: 2, IngressLimits: testIngressLimits(1 << 20), Usage: meter})
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
