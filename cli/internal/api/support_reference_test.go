package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/supportref"
)

func TestControlRequestPropagatesAndPreservesSupportReference(t *testing.T) {
	clientReference := "pb-0123456789abcdef0123456789abcdef"
	serverReference := "pb-fedcba9876543210fedcba9876543210"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get(supportref.Header); got != clientReference {
			t.Errorf("Support-Reference = %q", got)
		}
		writer.Header().Set(supportref.Header, serverReference)
		writer.WriteHeader(http.StatusConflict)
		_, _ = writer.Write([]byte(`{"error":{"code":"conflict","message":"conflict"}}`))
	}))
	defer server.Close()

	ctx := supportref.WithContext(context.Background(), clientReference)
	err := New(server.URL, config.Credential{}, server.Client()).do(ctx, http.MethodGet, "/v1/test", nil, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.SupportReference != serverReference {
		t.Fatalf("error = %#v", err)
	}
}

func TestSupportReferenceIsNotSentToObjectStorage(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get(supportref.Header); got != "" {
			t.Errorf("Support-Reference leaked to upload host: %q", got)
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	intent := DiagnosticUploadIntent{
		Schema: DiagnosticUploadIntentSchemaV1, IntentID: "diag_123", CorrelationID: "pb-0123456789abcdef0123456789abcdef",
		State: "pending", ExpiresAt: time.Now().UTC().Add(time.Minute), UploadMethod: http.MethodPut,
		UploadURL: server.URL, UploadHeaders: map[string]string{"Content-Type": "application/octet-stream"},
	}
	client := New("https://control.invalid", config.Credential{}, server.Client())
	ctx := supportref.WithContext(context.Background(), "pb-fedcba9876543210fedcba9876543210")
	if err := client.UploadDiagnosticBundle(ctx, intent, strings.NewReader("x"), 1); err != nil {
		t.Fatal(err)
	}
}

func TestControlErrorFallsBackToInvocationSupportReference(t *testing.T) {
	clientReference := "pb-0123456789abcdef0123456789abcdef"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set(supportref.Header, "invalid")
		writer.WriteHeader(http.StatusInternalServerError)
		_, _ = writer.Write([]byte(`{"error":{"code":"internal","message":"failed"}}`))
	}))
	defer server.Close()

	err := New(server.URL, config.Credential{}, server.Client()).do(supportref.WithContext(context.Background(), clientReference), http.MethodGet, "/v1/test", nil, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.SupportReference != clientReference {
		t.Fatalf("error = %#v", err)
	}
}

func TestControlErrorAcceptsNestedSupportReference(t *testing.T) {
	serverReference := "pb-fedcba9876543210fedcba9876543210"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusInternalServerError)
		_, _ = writer.Write([]byte(`{"error":{"code":"internal","message":"failed","support_reference":"` + serverReference + `"}}`))
	}))
	defer server.Close()
	err := New(server.URL, config.Credential{}, server.Client()).do(context.Background(), http.MethodGet, "/v1/test", nil, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.SupportReference != serverReference {
		t.Fatalf("error = %#v", err)
	}
}
