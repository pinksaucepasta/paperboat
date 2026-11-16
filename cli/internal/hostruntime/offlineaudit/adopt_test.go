package offlineaudit

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type fixedToken struct{}

func (fixedToken) Token(context.Context) (string, error) { return "machine-token", nil }

type fixedProof struct{}

func (fixedProof) Proof(context.Context, string, string, string, []byte) ([]byte, error) {
	return []byte("proof"), nil
}

// adopterAgainst answers the registration requests with statuses in turn (the last one repeats) and records them.
func adopterAgainst(t *testing.T, statuses ...int) (*Adopter, *Queue, func() []map[string]string) {
	t.Helper()
	var mu sync.Mutex
	var bodies []map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]string
		_ = json.Unmarshal(raw, &body)
		if r.URL.Path != "/v1/machines/machine_1/offline-terminal-sessions" || r.Header.Get("Authorization") != "Bearer machine-token" || r.Header.Get("X-Paperboat-Machine-Proof") == "" {
			t.Errorf("request %s with auth %q proof %q", r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("X-Paperboat-Machine-Proof"))
		}
		mu.Lock()
		defer mu.Unlock()
		bodies = append(bodies, body)
		status := statuses[min(len(bodies)-1, len(statuses)-1)]
		w.WriteHeader(status)
	}))
	t.Cleanup(server.Close)
	queue, err := Open(t.TempDir()+"/audit.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	adopter := &Adopter{Queue: queue.Adopt, ServerURL: server.URL, MachineID: "machine_1", Client: server.Client(), Tokens: fixedToken{}, Proofs: fixedProof{},
		OperationID: func() (string, error) { return "op", nil }, Interval: 10 * time.Millisecond}
	return adopter, queue, func() []map[string]string {
		mu.Lock()
		defer mu.Unlock()
		return append([]map[string]string(nil), bodies...)
	}
}

func queueOffline(t *testing.T, queue *Queue, session, account string) {
	t.Helper()
	if err := queue.Adopt.Record(Event{Kind: KindStarted, Capability: "terminal", PeerAccountID: account, OperationID: session}); err != nil {
		t.Fatal(err)
	}
}

// A session opened offline is registered by the host itself: refused while the machine is not online on the server,
// then taken once it is, naming the session and its owner, and it leaves the queue only then.
func TestAdopterRegistersAnOfflineSessionOnceTheServerTakesIt(t *testing.T) {
	adopter, queue, bodies := adopterAgainst(t, http.StatusConflict, http.StatusConflict, http.StatusOK)
	queueOffline(t, queue, "terminal_offline_a", "acct_1")
	queueOffline(t, queue, "terminal_offline_b", "acct_2")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go adopter.Run(ctx)
	for queue.Adopt.Len() > 0 && ctx.Err() == nil {
		time.Sleep(5 * time.Millisecond)
	}
	got := bodies()
	if len(got) < 4 || got[0]["session_id"] != "terminal_offline_a" || got[0]["account_id"] != "acct_1" || got[2]["session_id"] != "terminal_offline_a" || got[3]["session_id"] != "terminal_offline_b" || got[3]["account_id"] != "acct_2" {
		t.Fatalf("registration requests = %v, want a retried until taken, then b", got)
	}
	if queue.Adopt.Len() != 0 {
		t.Fatalf("%d sessions left queued", queue.Adopt.Len())
	}
}

// A session the server will never take (it is gone: 404) is dropped so it cannot block the ones behind it.
func TestAdopterDropsASessionTheServerRefusesForGood(t *testing.T) {
	adopter, queue, bodies := adopterAgainst(t, http.StatusNotFound, http.StatusOK)
	queueOffline(t, queue, "terminal_offline_gone", "acct_1")
	queueOffline(t, queue, "terminal_offline_live", "acct_1")
	if retry := adopter.Pass(context.Background()); retry {
		t.Fatal("a pass with only answered sessions asked for a retry")
	}
	got := bodies()
	if len(got) != 2 || got[1]["session_id"] != "terminal_offline_live" || queue.Adopt.Len() != 0 {
		t.Fatalf("requests %v, %d left; want both asked once and the queue empty", got, queue.Adopt.Len())
	}
}

// The queue survives a restart of the host: the sessions are still there after reopening the backlog file.
func TestOfflineSessionsQueuedForRegistrationSurviveARestart(t *testing.T) {
	path := t.TempDir() + "/audit.json"
	queue, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	queueOffline(t, queue, "terminal_offline_a", "acct_1")
	reopened, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Adopt.Oldest(10); len(got) != 1 || got[0].OperationID != "terminal_offline_a" || got[0].PeerAccountID != "acct_1" || !reopened.Adopt.HasOperation("terminal_offline_a") {
		t.Fatalf("reopened queue = %+v", got)
	}
}
