package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/diagnostics"
	"github.com/pinksaucepasta/paperboat/server/internal/resourceusage"
)

func TestUsageRejectsInvalidPagingBeforeQuery(t *testing.T) {
	for _, query := range []string{"limit=0", "limit=201", "offset=-1", "path_limit=33", "path_offset=-1", "limit=1&limit=2", "resource_route_id=&resource_route_id=x", "machine_id=&machine_id=x", "unknown=true"} {
		r := httptest.NewRequest("GET", "/v1/usage?start=2026-10-08T00:00:00Z&end=2026-10-08T01:00:00Z&"+query, nil)
		r = r.WithContext(context.WithValue(r.Context(), authContextKey{}, principal{User: auth.User{ID: "account"}}))
		w := httptest.NewRecorder()
		resourceUsageGet(nil)(w, r)
		if w.Code != 400 {
			t.Fatalf("query %s status %d", query, w.Code)
		}
	}
}

// The largest valid batch (MaxReports reports with every string at its field limit) is over 64 KiB; the size gate
// must let it reach machine proof (401), not refuse it as invalid (400).
func TestUsageNativeAcceptsLargeBatchPastSizeGate(t *testing.T) {
	start := time.Now().UTC().Truncate(time.Minute).Add(-2 * time.Minute)
	batch := resourceusage.NativeBatch{}
	for range resourceusage.MaxReports {
		batch.Reports = append(batch.Reports, resourceusage.NativeReport{AccessSessionID: strings.Repeat("s", 256), StreamID: strings.Repeat("t", 128), NodeID: strings.Repeat("n", 256), PeerAccountID: strings.Repeat("p", 256), Consumer: "ssh", Mode: "relay", IntervalStart: start, IntervalEnd: start.Add(time.Minute), UploadBytes: resourceusage.MaxCounter, DownloadBytes: resourceusage.MaxCounter})
	}
	body, err := json.Marshal(batch)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) <= 64<<10 || len(body) > maxNativeUsageBody {
		t.Fatalf("batch is %d bytes; it must exceed 64 KiB and fit the %d byte cap", len(body), maxNativeUsageBody)
	}
	r := httptest.NewRequest("POST", "/v1/usage/native", bytes.NewReader(body))
	w := httptest.NewRecorder()
	resourceUsageNative(nil, nil)(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("largest valid batch status %d body %s; want 401 from machine proof, not a size refusal", w.Code, w.Body.String())
	}
}

// The body is read before machine proof, so an unauthenticated caller must not make the server buffer more than the cap.
type countingZeroBody struct{ read int }

func (b *countingZeroBody) Read(p []byte) (int, error) {
	clear(p)
	b.read += len(p)
	return len(p), nil
}
func (*countingZeroBody) Close() error { return nil }

func TestUsageNativeBoundsPreAuthBodyRead(t *testing.T) {
	body := &countingZeroBody{}
	r := httptest.NewRequest("POST", "/v1/usage/native", nil)
	r.Body = body
	w := httptest.NewRecorder()
	resourceUsageNative(nil, nil)(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("oversized unauthenticated body status %d, want 400", w.Code)
	}
	if limit := resourceusage.MaxReports*2<<10 + 32<<10; body.read > limit {
		t.Fatalf("read %d bytes before machine proof; want at most %d", body.read, limit)
	}
}

func TestUsageNativeReadFailureReportsOriginalCauseSafely(t *testing.T) {
	const canary = "PRIVATE_USAGE_BODY_CANARY"
	err := errors.Join(context.Canceled, syscall.EIO, errors.New(canary))
	request, logs, reference := diagnosticRequest("/v1/usage/native")
	request.Method = http.MethodPost
	request.Body = failingUsageBody{err: err}
	request = request.WithContext(diagnostics.WithCorrelationID(request.Context(), "correlation_usage_body_01"))
	response := httptest.NewRecorder()
	resourceUsageNative(nil, nil)(response, request)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "usage_unavailable") || strings.Contains(response.Body.String(), canary) {
		t.Fatalf("unsafe or unhelpful response: %s", response.Body.String())
	}
	var event struct {
		Operation        string `json:"operation"`
		Stage            string `json:"stage"`
		Code             string `json:"code"`
		Cause            string `json:"cause"`
		Errno            int    `json:"errno"`
		HTTPStatus       int    `json:"http_status"`
		SupportReference string `json:"support_reference"`
		CorrelationID    string `json:"correlation_id"`
	}
	if err := json.Unmarshal(logs.Bytes(), &event); err != nil {
		t.Fatalf("expected one safe failure event: %v; logs=%q", err, logs.String())
	}
	if event.Operation != "http_other" || event.Stage != "http_handler" || event.Code != "usage_unavailable" || event.Cause != "system_error" || event.Errno != int(syscall.EIO) || event.HTTPStatus != http.StatusServiceUnavailable || event.SupportReference != reference || event.CorrelationID != "correlation_usage_body_01" {
		t.Fatalf("fault event = %#v", event)
	}
	if strings.Contains(logs.String(), canary) {
		t.Fatal("private request body error escaped into logs")
	}

	canceledRequest, canceledLogs, _ := diagnosticRequest("/v1/usage/native")
	canceledRequest.Method = http.MethodPost
	canceledRequest.Body = failingUsageBody{err: context.Canceled}
	resourceUsageNative(nil, nil)(httptest.NewRecorder(), canceledRequest)
	if canceledLogs.Len() != 0 {
		t.Fatalf("pure request cancellation emitted an operational fault: %s", canceledLogs.String())
	}
}

type failingUsageBody struct{ err error }

func (b failingUsageBody) Read([]byte) (int, error) { return 0, b.err }
func (failingUsageBody) Close() error               { return nil }
