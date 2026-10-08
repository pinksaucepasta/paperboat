//go:build darwin || linux || windows

package runtime

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/filetransfer"
)

type dedicatedTestTokenSource struct{}

func (dedicatedTestTokenSource) Token(context.Context) (string, error) { return "token", nil }

type dedicatedTestProofSource struct{}

func (*dedicatedTestProofSource) Proof(context.Context, string, string, string, []byte) ([]byte, error) {
	return []byte("proof"), nil
}

func TestRuntimeObservationUpdatesTransferPolicyWithoutTunnel(t *testing.T) {
	policy := filetransfer.DefaultPolicy
	policy.MaxFileBytes = 1024
	policy.MaxBatchBytes = 10240
	response := map[string]any{"data": map[string]any{"accepted": true, "file_transfer_policy": policy}}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Paperboat-Machine-Proof") == "" || r.Header.Get("Authorization") == "" {
			t.Error("missing machine authentication")
		}
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()
	policies := &filetransfer.PolicyStore{}
	if policies.Current().MaxBatchFiles != 0 {
		t.Fatal("uninitialized policy admits transfers")
	}
	sender := &runtimeObservationSender{endpoint: server.URL, tokens: dedicatedTestTokenSource{}, proofs: &dedicatedTestProofSource{}, operationID: func() (string, error) { return "operation_1", nil }, environmentID: "environment_1", machineID: "machine_1", client: server.Client(), transferPolicy: policies}
	if err := sender.Send(t.Context()); err != nil {
		t.Fatal(err)
	}
	if policies.Current() != policy {
		t.Fatal("server policy not applied")
	}
	for _, body := range []string{`{}`, `{"data":{"file_transfer_policy":null}}`, `{"data":{"file_transfer_policy":{"max_file_bytes":999999999999}}}`} {
		if applyRuntimeTransferPolicy([]byte(body), policies) == nil {
			t.Fatal("missing/invalid policy accepted")
		}
		if policies.Current() != policy {
			t.Fatal("invalid response weakened established limits")
		}
	}
}
