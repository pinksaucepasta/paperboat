package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/config"
)

type rejectingMachineAuth struct {
	machineAuthTestSource
	rejected []string
}

func (s *rejectingMachineAuth) CredentialRejected(token string) { s.rejected = append(s.rejected, token) }

// stg-m1: the server refused the machine credential on every machine-proof call, yet nothing told the source,
// so it kept serving the refused credential. A 401 on a machine-authenticated request now reports it.
func TestMachineRequestRefusedWith401ReportsTheCredential(t *testing.T) {
	status := http.StatusUnauthorized
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":{"code":"machine_control_credential_invalid","message":"rejected"}}`))
	}))
	defer server.Close()
	client := New(server.URL, config.Credential{}, server.Client())
	source := &rejectingMachineAuth{}
	client.SetMachineAuth(source)
	_ = client.ReportNativeUsage(context.Background(), []NativeUsageReport{}, 0, 0, false)
	if len(source.rejected) != 1 || source.rejected[0] != "machine-token" {
		t.Fatalf("rejected=%v, want the machine token reported once", source.rejected)
	}
	status = http.StatusServiceUnavailable
	_ = client.ReportNativeUsage(context.Background(), []NativeUsageReport{}, 0, 0, false)
	if len(source.rejected) != 1 {
		t.Fatal("a non-401 failure reported the credential as refused")
	}
}
