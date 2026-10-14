//go:build darwin || linux || windows

package runtime

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/server"
)

func TestTerminalJoinSenderSignsExactBodyAndRequiresReceipt(t *testing.T) {
	for _, result := range []struct {
		status int
		body   string
		ok     bool
	}{{202, `{"data":{"recorded":true}}`, true}, {202, `{"data":{}}`, false}, {503, `{"error":"unavailable"}`, false}} {
		proofs := &terminalJoinTestCredentials{}
		endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
			}
			if r.URL.Path != "/v1/runtime-observations" || r.Header.Get("Authorization") != "Bearer helper-identity" || r.Header.Get("X-Paperboat-Machine-Proof") != base64.RawURLEncoding.EncodeToString([]byte("proof")) || !bytes.Equal(raw, proofs.body) {
				t.Error("request did not preserve signed host/body")
			}
			for _, part := range []string{`"environment_id":"env_a"`, `"resource_id":"machine_a"`, `"access_session_id":"access_a"`, `"terminal_session_id":"session_a"`, `"attachment_id":"att_a"`} {
				if !bytes.Contains(raw, []byte(part)) {
					t.Error("missing exact join binding")
				}
			}
			if bytes.Contains(raw, []byte(`"environment":`)) || bytes.Contains(raw, []byte(`"runtime_diagnostics":`)) {
				t.Error("join changed unrelated observations")
			}
			w.WriteHeader(result.status)
			_, _ = io.WriteString(w, result.body)
		}))
		sender := &runtimeObservationSender{endpoint: endpoint.URL + "/v1/runtime-observations", tokens: proofs, proofs: proofs, operationID: func() (string, error) { return "op_join", nil }, environmentID: "env_a", machineID: "machine_a", reporterVersion: "test", client: endpoint.Client()}
		err := sender.RecordTerminalJoin(context.Background(), server.TerminalJoin{AccessSessionID: "access_a", TerminalSessionID: "session_a", AttachmentID: "att_a"})
		endpoint.Close()
		if (err == nil) != result.ok {
			t.Fatalf("status=%d err=%v", result.status, err)
		}
	}
}

type terminalJoinTestCredentials struct{ body []byte }

func (*terminalJoinTestCredentials) Token(context.Context) (string, error) {
	return "helper-identity", nil
}
func (c *terminalJoinTestCredentials) Proof(_ context.Context, _ string, method, path string, body []byte) ([]byte, error) {
	if method != http.MethodPost || path != "/v1/runtime-observations" {
		return nil, errors.New("unexpected proof target")
	}
	c.body = append([]byte(nil), body...)
	return []byte("proof"), nil
}
