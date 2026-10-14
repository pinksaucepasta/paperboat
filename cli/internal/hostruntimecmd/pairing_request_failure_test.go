package hostruntimecmd

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
)

func TestPairingServerFailureNamesRequestAndStatusWithoutPrivateBody(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":{"code":"private-server-detail","message":"PRIVATE_ENROLLMENT_DETAIL"}}`)
	}))
	defer server.Close()
	_, err := bootstrap.CreatePairing(t.Context(), bootstrap.Config{
		ServerURL: server.URL, HTTP: server.Client(), Alias: "test-machine", WorkspaceRoot: t.TempDir(),
		EnrollmentToken: strings.Repeat("T", 26), Verifier: strings.Repeat("V", 32),
		PublicIdentityKey: base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize)),
	})
	if err == nil {
		t.Fatal("server refusal unexpectedly succeeded")
	}
	message := runtimeFailureMessage(err)
	if !strings.Contains(message, "pairing request") || !strings.Contains(message, "HTTP 503") || !strings.Contains(message, "retry") {
		t.Fatalf("server request outcome not actionable: %q", message)
	}
	if strings.Contains(message, "PRIVATE") || strings.Contains(message, "private-server-detail") {
		t.Fatal("server body entered public pairing diagnostics")
	}
}
