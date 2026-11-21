package derpquic

import (
	"log/slog"
	"os"
	"strings"
	"testing"

	"tailscale.com/types/key"
)

func TestFenceRefusalLogsVerifiedAccountAndEndpoint(t *testing.T) {
	f := newFixture(t)
	s, address, _ := f.start("127.0.0.1:0")
	cert := certificate(t)
	grant := f.grant(key.NewNode().Public(), cert, "fenced-endpoint")
	if err := s.Revoke(grant.AccountID, grant.EndpointID, grant.Generation+1); err != nil {
		t.Fatal(err)
	}

	logs, err := os.CreateTemp(t.TempDir(), "refusals")
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	defer slog.SetDefault(previous)

	if err := f.client(address, cert, grant).Connect(t.Context()); err == nil {
		t.Fatal("fenced grant was admitted")
	}
	data, err := os.ReadFile(logs.Name())
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{"relay admission refused", "code=1", "admit: fence", "account_id=" + grant.AccountID, "endpoint_id=" + grant.EndpointID} {
		if !strings.Contains(got, want) {
			t.Errorf("refusal log missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, f.token(grant)) {
		t.Fatal("refusal log exposed the signed credential")
	}
}
