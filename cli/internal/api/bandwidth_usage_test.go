package api

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/pinksaucepasta/paperboat/internal/config"
)

func TestNativeUsageSignsAuthenticatedReport(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		operation := r.Header.Get("Idempotency-Key")
		if _, err := uuid.Parse(strings.TrimPrefix(operation, "operation_")); err != nil {
			t.Errorf("missing valid operation ID: %q", operation)
		}
		body, _ := io.ReadAll(r.Body)
		proof, err := base64.RawURLEncoding.DecodeString(r.Header.Get("X-Paperboat-Machine-Proof"))
		if err != nil || string(proof) != operation+"|POST|/v1/usage/native|"+string(body) || r.Header.Get("Authorization") != "Bearer machine-token" {
			t.Error("report must authenticate and sign its exact route and body")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":{"accepted":0,"rejected_indices":[]}}`)
	}))
	defer server.Close()
	client := New(server.URL, config.Credential{}, server.Client())
	client.SetMachineAuth(machineAuthTestSource{})
	if err := client.ReportNativeUsage(context.Background(), []NativeUsageReport{}, 0, 0, false); err != nil {
		t.Fatal(err)
	}
}
