package bugreport

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

type closingUploadServer struct {
	serverStub
	failUpload error
}

func (s *closingUploadServer) UploadDiagnosticBundle(ctx context.Context, intent api.DiagnosticUploadIntent, reader io.Reader, count int64) error {
	if err := s.serverStub.UploadDiagnosticBundle(ctx, intent, reader, count); err != nil {
		return err
	}
	// Simulate an externally invalidated caller-owned descriptor, rather than
	// hiding the subsequent owned Close failure.
	if err := reader.(*os.File).Close(); err != nil {
		return err
	}
	return s.failUpload
}
func TestWorkflowRetainsCloseFailureAndConfirmedUpload(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "completed", true: "failed"}[failed], func(t *testing.T) {
			local := &localStub{bundle: testBundle(t, []byte("redacted"))}
			server := &closingUploadServer{}
			primary := errors.New("PRIVATE_UPLOAD_FAILURE")
			if failed {
				server.failUpload = primary
			}
			result, err := Run(t.Context(), Options{Upload: true, Input: bytes.NewReader(nil), Prompt: io.Discard, Local: local, Server: server})
			if !errors.Is(err, os.ErrClosed) || result.Uploaded == failed || !result.BundleCreated || strings.Contains(err.Error(), "PRIVATE") {
				t.Fatal("cleanup erased confirmed result/cause or exposed content")
			}
			var stage *StageError
			if !errors.As(err, &stage) {
				t.Fatal("stage lost")
			}
			if failed && (!errors.Is(err, primary) || stage.Stage != "upload bundle") {
				t.Fatal("cleanup replaced original failure")
			}
			if !failed && (stage.Stage != "close local bundle" || stage.Result.Uploaded != true || result.Validate() != nil || stage.Error() != "diagnostic upload completed; closing the local bundle failed") {
				t.Fatal("completion facts lost")
			}
			// Preserved local bundle remains usable on a fresh workflow.
			if next, err := Run(t.Context(), Options{Input: bytes.NewReader(nil), Prompt: io.Discard, Local: local}); err != nil || !next.BundleCreated {
				t.Fatal("fresh workflow did not recover")
			}
		})
	}
}
