package control

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/pinksaucepasta/paperboat/edge/internal/usage"
)

// ErrUsageRefused marks a report the control plane refused for good: retrying the same bytes cannot succeed.
var ErrUsageRefused = errors.New("usage report refused by the control plane")

// permanentUsageStatus is a 4xx answer that will not change on a retry; 401, 403, 404, 408, 425 and 429 can
// (credentials, a node still registering, a busy server), so those and every 5xx or timeout stay queued.
func permanentUsageStatus(status int) bool {
	switch status {
	case http.StatusBadRequest, http.StatusConflict, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return true
	}
	return false
}

// DeliverNext sends the report at the head of the queue. A permanent refusal moves it to the queue's dead
// letters (kept, counted and logged with its route) and returns ErrUsageRefused with delivered=true, so the
// caller goes on with the next report; any other error leaves it queued for the next try.
func DeliverNext(ctx context.Context, queue *usage.Queue, sink UsageSink) (UsageResult, bool, error) {
	if queue == nil || sink == nil {
		return UsageResult{}, false, nil
	}
	report, ok := queue.Next()
	if !ok {
		return UsageResult{}, false, nil
	}
	result, err := sink.ReportUsage(ctx, UsageReport{OperationID: report.OperationID, Key: report.Key, Bytes: report.Bytes, Interval: report.Interval, Payload: append([]byte(nil), report.Payload...)})
	if err != nil {
		var failure *RequestFailure
		if errors.As(err, &failure) && permanentUsageStatus(failure.Status) {
			reason := fmt.Sprintf("the control plane answered HTTP %d (support reference %s)", failure.Status, failure.SupportReference)
			if queue.Refuse(report.OperationID, reason, time.Now()) {
				slog.WarnContext(ctx, "edge usage report refused for good and kept as a dead letter", "route_id", report.Key.Route, "operation_id", report.OperationID, "reason", reason)
			}
			return UsageResult{}, true, fmt.Errorf("%w: %s", ErrUsageRefused, reason)
		}
		return UsageResult{}, true, err
	}
	if result.QuotaExhausted {
		queue.ExhaustAuthority(report.Key.Authority, report.Interval[1])
	}
	queue.Ack(report.OperationID)
	return result, true, nil
}
