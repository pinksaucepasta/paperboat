package recordingcontrol

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	wire "github.com/pinksaucepasta/paperboat/shared/recording"
)

type staticToken struct{}

func (staticToken) Token(context.Context) (string, error) { return "runtime-token", nil }

type recordingProofs struct{ bodies [][]byte }

func (p *recordingProofs) Proof(_ context.Context, _, _, _ string, body []byte) ([]byte, error) {
	p.bodies = append(p.bodies, body)
	return []byte("proof"), nil
}

func newFetcher(t *testing.T, handler http.HandlerFunc) (*Fetcher, *recordingProofs) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	proofs := &recordingProofs{}
	return &Fetcher{ServerURL: server.URL, Client: server.Client(), Tokens: staticToken{}, Proofs: proofs, OperationID: func() (string, error) { return "op_1", nil }}, proofs
}

// Every Paperboat answer wraps its payload in {"data": …}. The machine read the bare document, got a zero policy and
// never recorded on a real device.
func TestFetchUnwrapsTheServersDataEnvelope(t *testing.T) {
	want := policyWith(7, newKey(t, "team_1", 1, nil).key)
	fetcher, _ := newFetcher(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != wire.PolicyPath || r.Header.Get("Authorization") != "Bearer runtime-token" || r.Header.Get("X-Paperboat-Machine-Proof") == "" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": want, "unknown_future_field": 1})
	})
	got, err := fetcher.Fetch(context.Background())
	if err != nil || got.Version != 7 || !got.Enabled || got.TeamID != "team_1" || len(got.Keys) != 1 {
		t.Fatalf("policy %+v, %v", got, err)
	}
	empty, _ := newFetcher(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{}`) })
	if _, err := empty.Fetch(context.Background()); err == nil {
		t.Fatal("an answer without data was taken as a policy")
	}
}
