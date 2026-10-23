package app

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/backupreport"
	"github.com/pinksaucepasta/paperboat/server/internal/supportreport"
	"github.com/pinksaucepasta/paperboat/server/internal/workers"
)

func TestOperationalHealthWorkerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- operationalHealthWorker(nil, nil, nil)(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("health worker ignored cancellation")
	}
}

func TestWorkerHealthStartupFailureAndRecoverySemantics(t *testing.T) {
	for _, tt := range []struct {
		samples          []workers.ProgressSample
		healthy, warming bool
	}{
		{[]workers.ProgressSample{{State: workers.ProgressUnknown, Warming: true}}, false, true},
		{[]workers.ProgressSample{{State: workers.ProgressRunning, Warming: true}}, false, true},
		{[]workers.ProgressSample{{State: workers.ProgressCurrent, Healthy: true}}, true, false},
		{[]workers.ProgressSample{{State: workers.ProgressRunning, Healthy: true}}, true, false},
		{[]workers.ProgressSample{{State: workers.ProgressStale}}, false, false},
		{[]workers.ProgressSample{{State: workers.ProgressCanceled}}, false, false},
		{[]workers.ProgressSample{{State: workers.ProgressUnknown, Warming: true}, {State: workers.ProgressFailed}}, false, false},
	} {
		healthy, warming := publishWorkerHealth(nil, tt.samples)
		if healthy != tt.healthy || warming != tt.warming {
			t.Fatalf("got healthy=%v warming=%v for %+v", healthy, warming, tt.samples)
		}
	}
}

func TestOperationalHealthFailureIsDeduplicatedAndSamplingRecovers(t *testing.T) {
	store := reportingCaptureStore{records: make(chan supportreport.Record, 4)}
	reporter, err := supportreport.New(store, supportreport.Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer reporter.Close()
	var logs strings.Builder
	reporter.SetLogger(slog.New(slog.NewJSONHandler(&logs, nil)))
	reporterCtx, stopReporter := context.WithCancel(t.Context())
	reporterDone := make(chan error, 1)
	go func() { reporterDone <- reporter.Run(reporterCtx) }()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	private := errors.Join(syscall.EIO, context.Canceled, errors.New("PRIVATE_DATABASE_DETAIL"))
	unavailable := backupreport.HealthSnapshot{Backup: backupreport.EvidenceHealth{State: backupreport.HealthUnavailable}, Restore: backupreport.EvidenceHealth{State: backupreport.HealthUnavailable}}
	current := backupreport.HealthSnapshot{Backup: backupreport.EvidenceHealth{State: backupreport.HealthCurrent}, Restore: backupreport.EvidenceHealth{State: backupreport.HealthCurrent}}
	worker := operationalHealthWorkerWithCheck(5*time.Millisecond, func(context.Context) (backupreport.HealthSnapshot, error) {
		calls++
		if calls < 3 {
			return unavailable, private
		}
		cancel()
		return current, nil
	}, reporter, nil, workers.NewProgress(time.Minute))
	workerDone := make(chan error, 1)
	go func() { workerDone <- worker(ctx) }()
	select {
	case err := <-workerDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("health worker did not continue through failure and recovery")
	}
	if calls != 3 {
		t.Fatalf("continued health samples=%d, want failure, repeated failure, recovery", calls)
	}

	var observed [2]supportreport.Record
	for index := range observed {
		select {
		case observed[index] = <-store.records:
		case <-time.After(time.Second):
			t.Fatalf("missing local health observation %d", index)
		}
	}
	if observed[0].Code != "service_failed" || observed[0].Cause != "system_error" || observed[0].Errno != int(syscall.EIO) || observed[0].Outcome != "failed" || observed[0].Severity != "warning" || observed[0].Reference == "" {
		t.Fatalf("health failure lost safe cause or support reference: %+v", observed[0])
	}
	if observed[1].Code != "worker_recovered" || observed[1].Outcome != "success" || observed[1].Severity != "info" || observed[1].Reference != observed[0].Reference {
		t.Fatalf("health recovery did not reuse its support reference: %+v", observed[1])
	}
	select {
	case extra := <-store.records:
		t.Fatalf("repeated failure was not deduplicated: %+v", extra)
	default:
	}
	stopReporter()
	if err := <-reporterDone; err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`"cause":"system_error"`, `"errno":5`, `"support_reference":"` + observed[0].Reference, "backup health check failed", "backup health check recovered"} {
		if !strings.Contains(logs.String(), expected) {
			t.Errorf("local health log omitted %q: %s", expected, logs.String())
		}
	}
	if strings.Count(logs.String(), "backup health check failed") != 1 || strings.Contains(logs.String(), "PRIVATE_DATABASE_DETAIL") {
		t.Fatalf("repeated or private health failure logging: %s", logs.String())
	}
}
