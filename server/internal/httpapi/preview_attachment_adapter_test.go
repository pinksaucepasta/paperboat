package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"

	"github.com/pinksaucepasta/paperboat/server/internal/controlplane"
	"github.com/pinksaucepasta/paperboat/server/internal/previewattachment"
)

type machineProofErrorVerifier struct {
	claims controlplane.MachineRequestClaims
	err    error
}

func (v machineProofErrorVerifier) VerifyMachineRequest(context.Context, string, []byte, string, string, []byte) (controlplane.MachineRequestClaims, error) {
	return v.claims, v.err
}

func TestPreviewAttachmentAdapterPreservesVerifierOutageCause(t *testing.T) {
	cause := syscall.ECONNRESET
	verifier, err := NewPreviewAttachmentMachineProofVerifier(machineProofErrorVerifier{err: cause})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/previews/preview_1/attachment", nil)
	request.Header.Set("Authorization", "Bearer machine_credential")
	request.Header.Set("X-Paperboat-Machine-Identity", "machine_credential")
	request.Header.Set("X-Paperboat-Machine-Proof", "cHJvb2Y")
	_, err = verifier.VerifyMachineRequest(context.Background(), request, []byte(`{}`))
	if !errors.Is(err, cause) || errors.Is(err, previewattachment.ErrUnauthorized) {
		t.Fatalf("adapter error=%v; want original verifier outage without expected-rejection classification", err)
	}
}
