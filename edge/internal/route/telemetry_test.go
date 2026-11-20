package route

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/shared/health"
	sharedtelemetry "github.com/pinksaucepasta/paperboat/shared/telemetry"
	"strings"
	"sync"
	"testing"
	"time"

	edgetelemetry "github.com/pinksaucepasta/paperboat/edge/internal/telemetry"
)

type recordingRouteTelemetry struct {
	mu      sync.Mutex
	records []RouteTelemetryRecord
	err     error
	panic   bool
}

func (r *recordingRouteTelemetry) RecordRouteTelemetry(record RouteTelemetryRecord) error {
	if r.panic {
		panic("injected telemetry panic")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, record)
	return r.err
}

func (r *recordingRouteTelemetry) types() []LifecycleType {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]LifecycleType, len(r.records))
	for index, record := range r.records {
		result[index] = record.Type
	}
	return result
}

func TestGenerationRegistryTelemetryOrderingStaleSafetyAndForcedDrain(t *testing.T) {
	sink := &recordingRouteTelemetry{}
	at := time.Date(2026, 8, 31, 1, 2, 3, 0, time.UTC)
	registry, err := NewGenerationRegistryWithOptions(GenerationRegistryOptions{
		TelemetrySink: sink, TelemetryClock: func() time.Time { return at },
	})
	if err != nil {
		t.Fatal(err)
	}
	oldRule := matcherRule("route_old", "app.example.test", MatchExact, "/", 1)
	oldRule.RouteID, oldRule.ConnectorSessionID = "route_old", "session_old"
	activateMatcher(t, registry, 1, []RouteRule{oldRule})
	lease, _, err := registry.Acquire(context.Background(), "app.example.test", "/")
	if err != nil {
		t.Fatal(err)
	}
	newRule := matcherRule("route_new", "app.example.test", MatchExact, "/", 1)
	newRule.RouteID, newRule.ConnectorSessionID = "route_new", "session_new"
	if err := registry.StageGeneration(2, []RouteRule{newRule}); err != nil {
		t.Fatal(err)
	}
	if err := registry.MarkGenerationReady(2); err != nil {
		t.Fatal(err)
	}
	if err := registry.ActivateGeneration(context.Background(), 2, 5*time.Millisecond); !errors.Is(err, ErrDrainTimeout) {
		t.Fatalf("forced drain error = %v", err)
	}
	match, err := registry.Match("app.example.test", "/")
	if err != nil || match.Generation != 2 || match.Rule.ID != "route_new" {
		t.Fatalf("forced drain changed active route: %+v, %v", match, err)
	}
	if err := registry.StageGeneration(1, []RouteRule{oldRule}); !errors.Is(err, ErrGenerationStale) {
		t.Fatalf("stale stage error = %v", err)
	}
	match, err = registry.Match("app.example.test", "/")
	if err != nil || match.Generation != 2 {
		t.Fatalf("stale stage replaced active route: %+v, %v", match, err)
	}
	_ = lease.Close()

	wantPrefix := []LifecycleType{
		LifecycleStageAccepted, LifecycleGenerationReady, LifecycleActivated,
		LifecycleStreamAcquired,
		LifecycleStageAccepted, LifecycleGenerationReady, LifecycleActivated,
		LifecycleDrainStarted,
	}
	got := sink.types()
	if len(got) != len(wantPrefix)+4 || !equalLifecycleTypes(got[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("lifecycle prefix = %v, want %v plus four terminal events", got, wantPrefix)
	}
	positions := make(map[LifecycleType]int, 4)
	for index, event := range got[len(wantPrefix):] {
		if _, duplicate := positions[event]; duplicate {
			t.Fatalf("duplicate terminal lifecycle event %q in %v", event, got)
		}
		positions[event] = index
	}
	for _, required := range []LifecycleType{LifecycleDrainForced, LifecycleStageRejected, LifecycleStaleRejected, LifecycleStreamReleased} {
		if _, ok := positions[required]; !ok {
			t.Fatalf("missing terminal lifecycle event %q in %v", required, got)
		}
	}
	if positions[LifecycleStageRejected] > positions[LifecycleStaleRejected] {
		t.Fatalf("stale rejection preceded stage rejection: %v", got)
	}
}

func TestGenerationRegistryStreamAcquireAndReleaseTelemetry(t *testing.T) {
	sink := &recordingRouteTelemetry{}
	registry, err := NewGenerationRegistryWithOptions(GenerationRegistryOptions{TelemetrySink: sink})
	if err != nil {
		t.Fatal(err)
	}
	activateMatcher(t, registry, 1, []RouteRule{matcherRule("route_1", "app.example.test", MatchExact, "/", 1)})
	// A route holds any number of concurrent streams: there is no stream limit to overload.
	var leases []*StreamLease
	for range 20000 {
		lease, _, err := registry.Acquire(context.Background(), "app.example.test", "/")
		if err != nil {
			t.Fatal(err)
		}
		leases = append(leases, lease)
	}
	for _, lease := range leases {
		_ = lease.Close()
	}
	acquired := 0
	for _, kind := range sink.types() {
		if kind == LifecycleStreamAcquired {
			acquired++
		}
	}
	if acquired != len(leases) {
		t.Fatalf("%d stream_acquired events for %d streams", acquired, len(leases))
	}
	if err := registry.StageGeneration(2, []RouteRule{matcherRule("route_2", "app.example.test", MatchExact, "/", 1)}); err != nil {
		t.Fatal(err)
	}
	if err := registry.MarkGenerationReady(2); err != nil {
		t.Fatal(err)
	}
	if err := registry.ActivateGeneration(context.Background(), 2, time.Second); err != nil {
		t.Fatal(err)
	}
	types := sink.types()
	wantDrainTail := []LifecycleType{LifecycleStageAccepted, LifecycleGenerationReady, LifecycleActivated, LifecycleDrainStarted, LifecycleDrainCompleted}
	if len(types) < len(wantDrainTail) || !equalLifecycleTypes(types[len(types)-len(wantDrainTail):], wantDrainTail) {
		t.Fatalf("completed drain lifecycle = %v", types)
	}
}

func TestGenerationRegistryTelemetryFailureAndPanicNeverCorruptRouting(t *testing.T) {
	for _, test := range []struct {
		name string
		sink *recordingRouteTelemetry
	}{
		{"error", &recordingRouteTelemetry{err: errors.New("injected sink failure")}},
		{"panic", &recordingRouteTelemetry{panic: true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			registry, err := NewGenerationRegistryWithOptions(GenerationRegistryOptions{TelemetrySink: test.sink})
			if err != nil {
				t.Fatal(err)
			}
			activateMatcher(t, registry, 1, []RouteRule{matcherRule("route_1", "app.example.test", MatchExact, "/", 1)})
			match, err := registry.Match("app.example.test", "/")
			if err != nil || match.Generation != 1 {
				t.Fatalf("telemetry failure corrupted route: %+v, %v", match, err)
			}
			if registry.TelemetryDrops() != 3 {
				t.Fatalf("telemetry drops = %d, want 3", registry.TelemetryDrops())
			}
		})
	}
}

func TestCoreTelemetrySinkHealthMetricsRedactionAndSafeIDs(t *testing.T) {
	at := time.Date(2026, 8, 31, 1, 2, 3, 0, time.UTC)
	healthTracker, err := edgetelemetry.NewHealthTracker(func() time.Time { return at })
	if err != nil {
		t.Fatal(err)
	}
	metrics := edgetelemetry.NewMetrics()
	events, err := sharedtelemetry.NewEventLog(64)
	if err != nil {
		t.Fatal(err)
	}
	sink, err := NewCoreTelemetrySink(healthTracker, metrics, events)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := NewGenerationRegistryWithOptions(GenerationRegistryOptions{
		TelemetrySink: sink, TelemetryClock: func() time.Time { return at },
	})
	if err != nil {
		t.Fatal(err)
	}
	rule := matcherRule("route_01", "customer-secret.example.com", MatchExact, "/", 1)
	rule.RouteID = "route_01"
	rule.TunnelID = "tunnel_customer.example.com"
	rule.ConnectorID = "connector_02"
	rule.ConnectorSessionID = "session_03"
	rule.ConfigGeneration = 11
	rule.RouteGeneration = 12
	activateMatcher(t, registry, 1, []RouteRule{rule})
	lease, _, err := registry.Acquire(context.Background(), "customer-secret.example.com", "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = lease.Close()
	if registry.TelemetryDrops() != 0 {
		t.Fatalf("core telemetry drops = %d", registry.TelemetryDrops())
	}
	snapshot := healthTracker.Snapshot()
	if snapshot.Dimensions.Config.Status != health.StatusReady || snapshot.Dimensions.Route.Status != health.StatusReady {
		t.Fatalf("health was not recovered: %+v", snapshot.Dimensions)
	}
	metricJSON, err := metrics.JSON()
	if err != nil {
		t.Fatal(err)
	}
	eventSnapshot := events.Snapshot()
	if len(eventSnapshot) != 5 {
		t.Fatalf("event count = %d, want 5", len(eventSnapshot))
	}
	eventJSON := make([]string, 0, len(eventSnapshot))
	for _, event := range eventSnapshot {
		body, err := event.JSON()
		if err != nil {
			t.Fatal(err)
		}
		eventJSON = append(eventJSON, string(body))
	}
	combined := string(metricJSON) + strings.Join(eventJSON, "")
	for _, unsafe := range []string{"customer-secret.example.com", "tunnel_customer.example.com"} {
		if strings.Contains(combined, unsafe) {
			t.Fatalf("unsafe high-cardinality value %q in telemetry: %s", unsafe, combined)
		}
	}
	if !strings.Contains(combined, "route_01") || !strings.Contains(combined, "connector_02") || !strings.Contains(combined, "session_03") {
		t.Fatalf("safe actionable IDs missing: %s", combined)
	}
	if eventSnapshot[0].Generations.Config != 11 || eventSnapshot[0].Generations.Route != 12 {
		t.Fatalf("generation identity missing: %+v", eventSnapshot[0].Generations)
	}
}

func equalLifecycleTypes(left, right []LifecycleType) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
