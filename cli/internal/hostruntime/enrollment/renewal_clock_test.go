package enrollment

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/serverclock"
)

// TestRenewalWorksOnAMachineTenMinutesBehind: the server's clock is 10 minutes ahead and accepts a
// proof only within a minute of it (as the control plane does). The machine learns the server's clock
// from the enrollment answer's Date, so it sees its credential due for renewal and signs the renewal
// proof with the server's time (F21-02).
func TestRenewalWorksOnAMachineTenMinutesBehind(t *testing.T) {
	skew := 10 * time.Minute
	serverNow := func() time.Time { return time.Now().UTC().Add(skew) }
	t.Cleanup(func() { serverclock.Observe(time.Now().UTC().Format(http.TimeFormat), time.Now(), time.Now()) })
	renewedCredential := "renewed-helper-credential-012345678901234567890"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", serverNow().Format(http.TimeFormat))
		switch r.URL.Path {
		case "/v1/helper-enrollments":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"helper_id": "hlp_1", "machine_id": "machine_1", "environment_id": "env_1", "credential": "initial-helper-credential-012345678901234567890", "expires_at": serverNow().Add(5 * time.Minute)}})
		case "/v1/helper-identity-renewals":
			var envelope struct{ Payload string }
			var claims struct {
				IssuedAt time.Time `json:"issued_at"`
			}
			raw, err := base64.RawURLEncoding.DecodeString(r.Header.Get("X-Paperboat-Machine-Proof"))
			if err == nil {
				err = json.Unmarshal(raw, &envelope)
			}
			if err == nil {
				raw, err = base64.RawURLEncoding.DecodeString(envelope.Payload)
			}
			if err == nil {
				err = json.Unmarshal(raw, &claims)
			}
			if err != nil || claims.IssuedAt.Sub(serverNow()).Abs() > time.Minute {
				w.WriteHeader(http.StatusBadRequest)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "clock_skew"}})
				t.Errorf("renewal proof issued_at=%v, server time %v (err %v)", claims.IssuedAt, serverNow(), err)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"helper_id": "hlp_1", "machine_id": "machine_1", "environment_id": "env_1", "credential": renewedCredential, "expires_at": serverNow().Add(time.Hour)}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	root := t.TempDir()
	if err := makeEnrollmentStateRootPrivateForTest(root); err != nil {
		t.Fatal("protect enrollment test state root")
	}
	client, _ := NewClient(server.Client().Transport, time.Second)
	if _, err := client.Enroll(context.Background(), Config{ControlURL: server.URL, StateRoot: root, EnrollmentCredential: "grant-credential-012345678901234567890"}); err != nil {
		t.Fatal(err)
	}
	if got := serverclock.Offset(); (got - skew).Abs() > 5*time.Second {
		t.Fatalf("learned offset %v, want about %v", got, skew)
	}
	// No Clock: the production default.
	source, err := NewRenewingTokenSource(RenewingTokenConfig{ControlURL: server.URL, StateRoot: root, Transport: server.Client().Transport, RenewBefore: 10 * time.Minute, Timeout: time.Second, OperationID: func() (string, error) { return "op_renew_0001", nil }})
	if err != nil {
		t.Fatal(err)
	}
	token, err := source.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if token != renewedCredential {
		t.Fatalf("token=%q: the credential 5 minutes from expiry on the server's clock was not renewed", token)
	}
}
