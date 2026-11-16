package control

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/pinksaucepasta/paperboat/edge/internal/usage"
)

// failingSink answers each report by its operation id with a prepared error (nil = accepted).
type failingSink map[string]error

func (s failingSink) ReportUsage(_ context.Context, report UsageReport) (UsageResult, error) {
	if err := s[report.OperationID]; err != nil {
		return UsageResult{}, err
	}
	return UsageResult{Disposition: "accounted"}, nil
}

func queuedReports(t *testing.T, ids ...string) *usage.Queue {
	t.Helper()
	q, err := usage.NewQueue(8, 1<<10)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if err := q.Enqueue(usage.Report{OperationID: id, Key: usage.Key{Node: "n", Route: "route-" + id}, Bytes: 1, Payload: []byte("signed")}); err != nil {
			t.Fatal(err)
		}
	}
	return q
}

func TestRefusedUsageReportDoesNotBlockTheReportsBehindIt(t *testing.T) {
	q := queuedReports(t, "op_poison", "op_next")
	sink := failingSink{"op_poison": &RequestFailure{Path: "/v1/edge/usage-reports", Status: http.StatusBadRequest, SupportReference: "support_x", Err: ErrControlUnavailable}}
	if _, delivered, err := DeliverNext(context.Background(), q, sink); !delivered || !errors.Is(err, ErrUsageRefused) {
		t.Fatalf("poison report: delivered=%v err=%v", delivered, err)
	}
	dead := q.DeadLetters()
	if len(dead) != 1 || dead[0].Report.OperationID != "op_poison" || dead[0].Report.Key.Route != "route-op_poison" || dead[0].Reason == "" {
		t.Fatalf("dead letters = %+v", dead)
	}
	if result, delivered, err := DeliverNext(context.Background(), q, sink); err != nil || !delivered || result.Disposition != "accounted" {
		t.Fatalf("next report: %+v delivered=%v err=%v", result, delivered, err)
	}
	if stats := q.Stats(); stats.Reports != 0 || stats.DeadLetters != 1 || stats.Refused != 1 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestTransientUsageErrorsStayQueuedAndAreRetried(t *testing.T) {
	for _, failure := range []error{
		&RequestFailure{Status: http.StatusServiceUnavailable, Err: ErrControlUnavailable},
		&RequestFailure{Status: http.StatusTooManyRequests, Err: ErrControlUnavailable},
		&RequestFailure{Status: http.StatusUnauthorized, Err: ErrControlUnavailable},
		&RequestFailure{Category: "transport", Err: ErrControlUnavailable, Cause: context.DeadlineExceeded},
	} {
		q := queuedReports(t, "op_one")
		sink := failingSink{"op_one": failure}
		if _, _, err := DeliverNext(context.Background(), q, sink); err == nil || errors.Is(err, ErrUsageRefused) || q.Len() != 1 || len(q.DeadLetters()) != 0 {
			t.Fatalf("%v: err=%v queued=%d dead=%d", failure, err, q.Len(), len(q.DeadLetters()))
		}
		delete(sink, "op_one")
		if _, delivered, err := DeliverNext(context.Background(), q, sink); err != nil || !delivered || q.Len() != 0 {
			t.Fatalf("%v: retry err=%v queued=%d", failure, err, q.Len())
		}
	}
}
