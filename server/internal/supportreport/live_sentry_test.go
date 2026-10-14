package supportreport

import (
	"context"
	"os"
	"sync"
	"syscall"
	"testing"
)

// This opt-in test qualifies the real reporting SDK and ingestion boundary.
// Database persistence and deployed service behavior have separate gates.
func TestLiveSentryFailureRecovery(t *testing.T) {
	dsn, ref := os.Getenv("PB_LIVE_SENTRY_DSN"), os.Getenv("PB_LIVE_SENTRY_REF")
	if dsn == "" || ref == "" {
		t.Skip("set PB_LIVE_SENTRY_DSN and PB_LIVE_SENTRY_REF")
	}
	if !ValidReference(ref) {
		t.Fatal("PB_LIVE_SENTRY_REF must be a canonical support reference")
	}
	rate := 1.0
	store := &liveReceiptStore{}
	s, err := New(store, Config{Enabled: true, DSN: dsn, Release: "paperboat-server:validation-20261008", Environment: "sentry-validation", Region: "validation", Instance: "slot-01", TracesSampleRate: &rate})
	if err != nil {
		t.Fatal("live reporter configuration rejected")
	}
	defer s.Close()
	s.SetSnapshot(func() { s.Gauge("paperboat.database.connections", 1) })
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(runCtx) }()
	ctx, finish := s.Begin(context.Background(), "http_health", ref, "")
	fault := CaptureFromContext(ctx, "paperboat-server", "http_health", "http_handler", "control_unavailable", syscall.ECONNREFUSED)
	finish("failed")
	_, recovered := s.Begin(context.Background(), "http_health", ref, "")
	recovered("success")
	cancel()
	if err := <-done; err != nil {
		t.Fatal("reporter shutdown failed")
	}
	if fault.SupportReference != ref || fault.Errno != int(syscall.ECONNREFUSED) || store.saved(ref) != 1 {
		t.Fatal("failure evidence lost correlation, original errno, or persistence")
	}
	t.Logf("live_sentry correlated_reference=%s signals=error,log,trace,metric,drop_health", ref)
}

type liveReceiptStore struct {
	mu      sync.Mutex
	records []Record
}

func (s *liveReceiptStore) Save(_ context.Context, record Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, record)
	return nil
}

func (*liveReceiptStore) Lookup(context.Context, string, string, string, bool) ([]Record, error) {
	return nil, nil
}

func (*liveReceiptStore) Cleanup(context.Context) error { return nil }

func (s *liveReceiptStore) saved(ref string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, record := range s.records {
		if record.Reference == ref {
			n++
		}
	}
	return n
}
