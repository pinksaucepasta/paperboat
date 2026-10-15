package hostruntimecmd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
)

func TestPairingTokenRejectionsNameOutcomeAndPreserveOnlyExistingRecovery(t *testing.T) {
	cases := []struct {
		name, code, message        string
		status                     int
		definitive, cleanupFailure bool
	}{
		{"expired", "enrollment_token_expired", "token expired", 410, true, false},
		{"used", "enrollment_token_used", "already used", 409, true, false},
		{"invalid or revoked", "enrollment_token_invalid", "revoked or invalid", 400, true, false},
		{"platform", "pairing_platform_unsupported", "operating system", 400, true, false},
		{"status mismatch", "enrollment_token_expired", "HTTP 503", 503, false, false},
		{"lost response", "", "pairing request", 0, false, false},
		{"cleanup failure", "enrollment_token_invalid", "revoked or invalid", 400, true, true},
	}
	for _, tc := range cases {
		for _, existing := range []bool{false, true} {
			name := tc.name + "/fresh"
			if existing {
				name = tc.name + "/protected"
			}
			t.Run(name, func(t *testing.T) {
				root, err := filepath.EvalSymlinks(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				now := time.Now().UTC()
				public := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
				token := strings.Repeat("T", 26)
				tokenFile := filepath.Join(root, "token")
				if err := os.WriteFile(tokenFile, []byte(token), 0600); err != nil {
					t.Fatal(err)
				}
				server, attempts := pairingTokenRejectionServer(t, tc.status, tc.code)
				defer server.Close()
				if existing {
					record := bootstrap.NewResumeRecord(server.URL, public, token, "Laptop", "verifier-012345678901234567890123456789", now.Add(time.Hour))
					record.PairingStarted = true
					if err := bootstrap.SaveResume(root, record); err != nil {
						t.Fatal(err)
					}
				}
				resume, resumeErr := bootstrap.LoadResume(root, server.URL, public, token, "Laptop", now)
				input := testOneShotResumeInput(root, server.URL, public, token, resume, resumeErr)
				input.TokenFile = tokenFile
				input.Config.HTTP = server.Client()
				operations := defaultOneShotResumeOperations()
				cleanupErr := errors.New("PRIVATE_CLEANUP_DETAIL")
				if tc.cleanupFailure {
					operations.ClearResume = func(string) error { return cleanupErr }
				}
				_, _, failure := resumeOneShotEnrollment(context.Background(), input, operations)
				if failure == nil {
					t.Fatal("rejected pairing succeeded")
				}
				message := runtimeFailureMessage(failure)
				if !strings.Contains(message, tc.message) {
					t.Errorf("rejection lost its outcome: %q", message)
				}
				if strings.Contains(message, "PRIVATE_") || strings.Contains(failure.Error(), "PRIVATE_") {
					t.Error("private response entered pairing diagnostics")
				}
				recovered, recoverErr := bootstrap.LoadResume(root, server.URL, public, token, "Laptop", now)
				if tc.cleanupFailure && !existing {
					if !errors.Is(failure, cleanupErr) || !strings.Contains(message, "remove fresh recovery state") {
						t.Error("cleanup failure lost its cause or recovery action")
					}
				}
				if !existing && tc.definitive && !tc.cleanupFailure {
					if !errors.Is(recoverErr, bootstrap.ErrResumeNotFound) {
						t.Error("definitive fresh rejection retained an unfinished journal")
					}
				} else if recoverErr != nil || !recovered.PairingStarted || existing && recovered.Verifier != resume.Verifier {
					t.Error("protected or ambiguous pairing lost its recovery binding")
				}
				if _, err := os.Stat(tokenFile); err != nil {
					t.Error("rejected request consumed its input token file")
				}
				if attempts.Load() != 1 {
					t.Errorf("single-use request attempted %d times", attempts.Load())
				}
			})
		}
	}
}

func pairingTokenRejectionServer(t *testing.T, status int, code string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	attempts := new(atomic.Int32)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/machines/pairings/installation" {
			w.WriteHeader(410)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "user_machine_installation_unavailable"}})
			return
		}
		if r.URL.Path != "/v1/machines/pairings" {
			t.Error("unexpected pairing endpoint")
			http.NotFound(w, r)
			return
		}
		attempts.Add(1)
		if status == 0 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
			return
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{
			"code": code, "message": "PRIVATE_TOKEN_RESPONSE", "request_id": "PRIVATE_REQUEST_REFERENCE",
		}})
	}))
	return server, attempts
}
