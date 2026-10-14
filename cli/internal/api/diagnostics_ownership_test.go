package api

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/errorreport"
)

func uploadTestIntent(target string) DiagnosticUploadIntent {
	return DiagnosticUploadIntent{Schema: DiagnosticUploadIntentSchemaV1, IntentID: "diag_0123456789abcdef", CorrelationID: "support_01234567-89ab-4def-8123-456789abcdef", State: "pending", ExpiresAt: time.Now().UTC().Add(time.Minute), UploadMethod: http.MethodPut, UploadURL: target, UploadHeaders: map[string]string{"Content-Type": "application/zip"}}
}

func TestDiagnosticUploadBorrowsFileAndRecovers(t *testing.T) {
	payload := []byte("redacted bundle")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(got, payload) {
			t.Error("upload bytes changed")
		}
		w.WriteHeader(200)
	}))
	defer server.Close()
	path := filepath.Join(t.TempDir(), "bundle")
	if err := os.WriteFile(path, payload, 0600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	client := New(server.URL, config.Credential{}, server.Client())
	for range 2 {
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			t.Fatal("borrowed file closed")
		}
		if err := client.UploadDiagnosticBundle(t.Context(), uploadTestIntent(server.URL), file, int64(len(payload))); err != nil {
			t.Fatal(err)
		}
		if _, err := file.Stat(); err != nil {
			t.Fatal("upload closed caller descriptor")
		}
	}
}

type uploadFailureBody struct{ readErr, closeErr error }

func (b uploadFailureBody) Read([]byte) (int, error) { return 0, b.readErr }
func (b uploadFailureBody) Close() error             { return b.closeErr }

func TestDiagnosticUploadRetainsStatusDrainCloseAndPrivateNetworkCauses(t *testing.T) {
	drain := errors.New("PRIVATE_BODY_CONTENT")
	closeFailure := syscall.EIO
	client := New("https://control.example", config.Credential{}, &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		r.Body.Close()
		return &http.Response{StatusCode: 503, Header: make(http.Header), Body: uploadFailureBody{drain, closeFailure}, Request: r}, nil
	})})
	err := client.UploadDiagnosticBundle(t.Context(), uploadTestIntent("https://objects.example/PRIVATE_SIGNED_URL"), bytes.NewReader([]byte("x")), 1)
	fault := errorreport.ProjectFault(t.Context(), "paperboat-cli", "file_transfer", "delivery", "transport_failed", err)
	if !errors.Is(err, drain) || !errors.Is(err, closeFailure) || fault.HTTPStatus != 503 || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatal("upload lost status/cleanup/privacy")
	}
	client = New("https://control.example", config.Credential{}, &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) { r.Body.Close(); return nil, syscall.ECONNREFUSED })})
	err = client.UploadDiagnosticBundle(t.Context(), uploadTestIntent("https://objects.example/PRIVATE_SIGNED_URL"), bytes.NewReader([]byte("x")), 1)
	if !errors.Is(err, syscall.ECONNREFUSED) || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatal("network cause lost or presigned URL exposed")
	}
}

func TestDiagnosticUploadFencesLateTransportReads(t *testing.T) {
	var borrowedBody io.ReadCloser
	client := New("https://control.example", config.Credential{}, &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		borrowedBody = r.Body
		// A transport may defer body closure until after RoundTrip returns.
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})})
	reader := strings.NewReader("x")
	if err := client.UploadDiagnosticBundle(t.Context(), uploadTestIntent("https://objects.example"), reader, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := borrowedBody.Read(make([]byte, 1)); !errors.Is(err, io.ErrClosedPipe) || reader.Len() != 1 {
		t.Fatal("late transport read consumed caller content")
	}
	borrowedBody.Close()
}

func TestDiagnosticUploadAuthorityPreservesPrivateURLParserCause(t *testing.T) {
	intent := uploadTestIntent("https://objects.example/PRIVATE_PRESIGNED_TOKEN%PR?signature=PRIVATE_SECRET")
	err := intent.validate(true)
	var parser *url.Error
	if !errors.As(err, &parser) || parser == nil || parser.Err == nil {
		t.Fatal("malformed authority lost original URL parser cause")
	}
	if err.Error() != "paperboat-server returned invalid diagnostic upload authority" || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatal("malformed authority exposed presigned URL")
	}
	client := New("https://control.example", config.Credential{}, nil)
	err = client.UploadDiagnosticBundle(t.Context(), intent, strings.NewReader("x"), 1)
	if !errors.As(err, &parser) || strings.Contains(err.Error(), "PRIVATE") {
		t.Fatal("public upload boundary discarded its authority parser cause")
	}
	intent.UploadURL = "https://objects.example/upload?signature=valid"
	if err := intent.validate(true); err != nil {
		t.Fatal("valid authority did not recover")
	}
}
