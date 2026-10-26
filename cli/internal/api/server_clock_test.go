package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/shared/serverclock"
)

// A machine whose clock runs 10 minutes ahead of the server: the server's answer teaches this process
// the server's clock, so a grant that is valid for 5 more server minutes is not judged expired by the
// local clock (Lane X #2, clock skew).
func TestAServerGrantIsJudgedByTheServerClock(t *testing.T) {
	serverNow := time.Now().Add(-10 * time.Minute)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", serverNow.UTC().Format(http.TimeFormat))
		w.Header().Set("Content-Type", "application/json")
		expires := serverNow.Add(5 * time.Minute).UTC().Format(time.RFC3339)
		_, _ = w.Write([]byte(`{"data":{"operation_id":"op_clock_skew","expires_at":"` + expires + `","environment":{"environment_id":"env_1"}}}`))
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { serverclock.Observe(time.Now().UTC().Format(http.TimeFormat), time.Now(), time.Now()) })
	client := New(server.URL, config.Credential{AccessToken: "token"}, server.Client())
	_, err := client.ConfigConflictConnection(context.Background(), "env_1", "rev_1", 1, "path", "remote", "machine_1", "cls_1")
	if err != nil && strings.Contains(err.Error(), "lifetime") {
		t.Fatalf("a valid grant was judged by the skewed local clock: %v", err)
	}
	if offset := serverclock.Offset(); offset > -9*time.Minute || offset < -11*time.Minute {
		t.Fatalf("server clock offset = %v, want about -10m", offset)
	}
}
