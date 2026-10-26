package offlineaudit

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fixedTokens struct{}

func (fixedTokens) Token(context.Context) (string, error) { return "runtime-token", nil }

type recordingProofs struct {
	mu    sync.Mutex
	paths []string
}

func (p *recordingProofs) Proof(_ context.Context, _, _, path string, _ []byte) ([]byte, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.paths = append(p.paths, path)
	return []byte("proof"), nil
}

// Every decision is on disk before Record returns, survives a restart, and a full backlog refuses
// the next session instead of dropping events.
func TestQueueRecordsDurablyAndRefusesWhenFull(t *testing.T) {
	path := filepath.Join(t.TempDir(), "offline-audit.json")
	now := time.Now()
	q, err := Open(path, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Record(Event{Kind: KindStarted, Capability: "exec", PeerEndpointID: "cli_1", PeerAccountID: "acct", OperationID: "op_1"}); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if events := reopened.Oldest(10); len(events) != 1 || events[0].EventID == "" || events[0].OperationID != "op_1" {
		t.Fatalf("after restart the backlog holds %+v", events)
	}
	q.events = make([]Event, MaxEvents)
	for i := range q.events {
		q.events[i].OccurredAt = now
	}
	if err := q.Record(Event{Kind: KindStarted}); !errors.Is(err, ErrFull) {
		t.Fatalf("a full backlog accepted another event: %v", err)
	}
	q.events = []Event{{OccurredAt: now.Add(-MaxAge)}}
	if err := q.Record(Event{Kind: KindStarted}); !errors.Is(err, ErrFull) {
		t.Fatalf("a backlog whose oldest event is 7 days old accepted another event: %v", err)
	}
}

// The uploader sends the agreed body in batches of at most 500 to the machine's own path, with the
// runtime credential and a machine proof, removes what the server accounted for, and keeps the
// rest when the server fails.
func TestUploaderSendsBatchesAndKeepsEventsOnFailure(t *testing.T) {
	var mu sync.Mutex
	var batches []int
	fail := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var request struct {
			Events []map[string]any `json:"events"`
		}
		decoder := json.NewDecoder(strings.NewReader(string(body)))
		decoder.DisallowUnknownFields()
		if r.Method != http.MethodPost || r.URL.Path != "/v1/machines/machine_1/audit-backlog" || r.Header.Get("Authorization") != "Bearer runtime-token" || r.Header.Get("X-Paperboat-Machine-Proof") == "" || decoder.Decode(&request) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for _, event := range request.Events {
			for _, field := range []string{"event_id", "kind", "capability", "peer_endpoint_id", "peer_account_id", "operation_id", "occurred_at"} {
				if event[field] == nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
			}
		}
		mu.Lock()
		defer mu.Unlock()
		if fail {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		batches = append(batches, len(request.Events))
		_ = json.NewEncoder(w).Encode(map[string]int{"accepted": len(request.Events) - 1, "duplicates": 1})
	}))
	defer server.Close()
	q, err := Open(filepath.Join(t.TempDir(), "offline-audit.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	for range 1200 {
		q.events = append(q.events, Event{EventID: "e", Kind: KindRefused, Capability: "terminal", PeerEndpointID: "cli_1", PeerAccountID: "acct", OperationID: "op", OccurredAt: time.Now(), Reason: ReasonNoScope})
	}
	proofs := &recordingProofs{}
	uploader := &Uploader{Queue: q, ServerURL: server.URL, MachineID: "machine_1", Client: server.Client(), Tokens: fixedTokens{}, Proofs: proofs, OperationID: func() (string, error) { return "operation_test", nil }}
	mu.Lock()
	fail = true
	mu.Unlock()
	if err := uploader.Flush(t.Context()); err == nil || q.Len() != 1200 {
		t.Fatalf("a failing server: err=%v, %d events left, want 1200", err, q.Len())
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	if err := uploader.Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if q.Len() != 0 || len(batches) != 3 || batches[0] != MaxBatch || batches[2] != 200 {
		t.Fatalf("batches %v, %d left", batches, q.Len())
	}
	if proofs.paths[0] != "/v1/machines/machine_1/audit-backlog" {
		t.Fatalf("proof signed path %q", proofs.paths[0])
	}
}
