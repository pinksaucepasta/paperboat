package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/server/internal/auth"
	"github.com/pinksaucepasta/paperboat/server/internal/releaseauthority"
	"github.com/pinksaucepasta/paperboat/server/internal/supportreport"
)

func TestReleaseAuthorityBodyReadFailuresRetainCauseAtHTTPBoundary(t *testing.T) {
	const private = "RELEASE_AUTHORITY_BODY_PRIVATE"
	for _, test := range []struct {
		name    string
		handler func(*releaseauthority.Service) http.HandlerFunc
	}{
		{name: "request", handler: releaseAuthorityRequestCreate},
		{name: "bundle", handler: releaseAuthorityBundleImport},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &supportTestStore{}
			reporter, err := supportreport.New(store, supportreport.Config{})
			if err != nil {
				t.Fatal(err)
			}
			reportCtx, cancelReports := context.WithCancel(context.Background())
			reportsDone := make(chan struct{})
			go func() { _ = reporter.Run(reportCtx); close(reportsDone) }()
			defer func() {
				cancelReports()
				<-reportsDone
			}()

			reference := supportreport.NewReference()
			var logs strings.Builder
			base := test.handler(nil)
			handler := requestID(correlationID(supportMiddleware(reporter, slog.New(slog.NewJSONHandler(&logs, nil)), base)))
			request := httptest.NewRequest(http.MethodPost, "/v1/admin/release-authority/"+test.name, nil)
			request.Header.Set("Support-Reference", reference)
			request.Header.Set("Correlation-Id", "correlation_release_authority_01")
			request.Header.Set("Request-Id", "request_release_authority_01")
			request.Header.Set("Idempotency-Key", "release-authority-operation-01")
			request.Body = releaseAuthorityReadError{err: errors.Join(context.Canceled, syscall.EIO, errors.New(private))}
			request.ContentLength = 1
			request = request.WithContext(context.WithValue(request.Context(), authContextKey{}, principal{User: auth.User{ID: "usr_release_admin"}}))

			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusServiceUnavailable {
				t.Fatalf("body read status=%d body=%s", response.Code, response.Body.String())
			}
			var body ErrorResponse
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error.Code != "internal_error" || body.Error.SupportReference != reference || body.Error.CorrelationID != "correlation_release_authority_01" || body.Error.RequestID != "request_release_authority_01" {
				t.Fatalf("body-read response lost safe request identity: %+v", body.Error)
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
				RequestID        string `json:"request_id"`
			}
			if err := json.Unmarshal([]byte(strings.TrimSpace(logs.String())), &event); err != nil {
				t.Fatalf("expected exactly one safe failure event: %v; logs=%q", err, logs.String())
			}
			if event.Operation != "http_admin" || event.Stage != "http_handler" || event.Code != "internal_error" || event.Cause != "system_error" || event.Errno != int(syscall.EIO) || event.HTTPStatus != http.StatusServiceUnavailable || event.SupportReference != reference || event.CorrelationID != "correlation_release_authority_01" || event.RequestID != "request_release_authority_01" {
				t.Fatalf("failure event lost original cause or request identity: %+v", event)
			}
			if strings.Contains(logs.String(), private) || strings.Contains(response.Body.String(), private) {
				t.Fatal("release-authority body-read failure exposed private error text")
			}

			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				store.mu.Lock()
				records := append([]supportreport.Record(nil), store.records...)
				store.mu.Unlock()
				if len(records) != 0 {
					if len(records) != 1 {
						t.Fatalf("captured %d support records, want one", len(records))
					}
					record := records[0]
					if record.Code != "internal_error" || record.Cause != "system_error" || record.Errno != int(syscall.EIO) || record.HTTPStatus != http.StatusServiceUnavailable || record.Reference != reference || record.CorrelationID != "correlation_release_authority_01" || record.RequestID != "request_release_authority_01" || record.Outcome != "failed" {
						t.Fatalf("support record lost original cause or request identity: %+v", record)
					}
					encoded, err := json.Marshal(record)
					if err != nil || strings.Contains(string(encoded), private) {
						t.Fatalf("private failure detail escaped into support record: %s (%v)", encoded, err)
					}
					return
				}
				time.Sleep(time.Millisecond)
			}
			t.Fatal("release-authority body-read cause was not captured")
		})
	}
}

func TestReleaseAuthorityMalformedAndOversizedBodiesStayClientErrors(t *testing.T) {
	for _, test := range []struct {
		name    string
		handler func(*releaseauthority.Service) http.HandlerFunc
		body    string
	}{
		{name: "request malformed", handler: releaseAuthorityRequestCreate, body: "{"},
		{name: "request oversized", handler: releaseAuthorityRequestCreate, body: strings.Repeat(" ", 4097)},
		{name: "bundle malformed", handler: releaseAuthorityBundleImport, body: "{"},
		{name: "bundle oversized", handler: releaseAuthorityBundleImport, body: strings.Repeat(" ", 64<<10+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/admin/release-authority", strings.NewReader(test.body))
			request.Header.Set("Idempotency-Key", "release-authority-operation-01")
			request = request.WithContext(context.WithValue(request.Context(), authContextKey{}, principal{User: auth.User{ID: "usr_release_admin"}}))
			response := httptest.NewRecorder()
			test.handler(nil).ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("malformed/oversized body status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

type releaseAuthorityReadError struct{ err error }

func (r releaseAuthorityReadError) Read([]byte) (int, error) { return 0, r.err }
func (releaseAuthorityReadError) Close() error               { return nil }

func TestReleaseAuthorityCanceledBodyReadIsQuiet(t *testing.T) {
	for _, handler := range []func(*releaseauthority.Service) http.HandlerFunc{releaseAuthorityRequestCreate, releaseAuthorityBundleImport} {
		request, logs, _ := diagnosticRequest("/v1/admin/release-authority")
		request.Method = http.MethodPost
		request.Body = releaseAuthorityReadError{err: context.Canceled}
		request = request.WithContext(context.WithValue(request.Context(), authContextKey{}, principal{User: auth.User{ID: "usr_release_admin"}}))
		response := httptest.NewRecorder()
		handler(nil)(response, request)
		if response.Body.Len() != 0 || logs.Len() != 0 {
			t.Fatal("canceled release authority request wrote a response or failure record")
		}
	}
}
