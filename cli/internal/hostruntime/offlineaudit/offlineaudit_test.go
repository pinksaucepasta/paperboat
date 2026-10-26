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

// F5-12: a recorded event keeps the id it names (an end refers to its start), the ssh fields reach the wire in
// the agreed shape, and the uploader spaces its uploads: a burst of events goes up in one request, and idle sends nothing.
func TestSSHEventsKeepTheirIDsAndUploadAtMostOncePerInterval(t *testing.T) {
	var mu sync.Mutex
	var uploads []time.Time
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var request struct {
			Events []json.RawMessage `json:"events"`
		}
		_ = json.Unmarshal(body, &request)
		mu.Lock()
		uploads = append(uploads, time.Now())
		bodies = append(bodies, string(body))
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]int{"accepted": len(request.Events)})
	}))
	defer server.Close()
	q, err := Open(filepath.Join(t.TempDir(), "offline-audit.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	uploader := &Uploader{Queue: q, ServerURL: server.URL, MachineID: "machine_1", Client: server.Client(), Tokens: fixedTokens{}, Proofs: &recordingProofs{}, OperationID: func() (string, error) { return "operation_test", nil }, MinInterval: 300 * time.Millisecond}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go uploader.Run(ctx)

	record := func(e Event) {
		t.Helper()
		if err := q.Record(e); err != nil {
			t.Fatal(err)
		}
	}
	record(Event{EventID: "session_1", Kind: KindSSHStarted, PeerEndpointID: "cli_1", PeerAccountID: "acct", Origin: OriginOnline, Generation: 3})
	waitFor := func(count int) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			n := len(uploads)
			mu.Unlock()
			if n >= count {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("waited for %d uploads", count)
	}
	waitFor(1)
	// Three more events arrive right after the first upload: they go up together, one interval later.
	record(Event{Kind: KindSSHEnded, PeerEndpointID: "cli_1", PeerAccountID: "acct", Origin: OriginOnline, SessionID: "session_1", Outcome: "closed", BytesIn: 7, BytesOut: 9})
	record(Event{Kind: KindSSHRefused, PeerEndpointID: "cli_1", PeerAccountID: "acct", Origin: OriginOffline, Reason: "busy"})
	record(Event{Kind: KindSSHRefused, PeerEndpointID: "cli_1", PeerAccountID: "acct", Origin: OriginOnline, Reason: "not_set_up"})
	waitFor(2)
	time.Sleep(600 * time.Millisecond) // idle: nothing more is sent
	mu.Lock()
	defer mu.Unlock()
	if len(uploads) != 2 || uploads[1].Sub(uploads[0]) < 250*time.Millisecond {
		t.Fatalf("%d uploads, second %v after the first", len(uploads), uploads[len(uploads)-1].Sub(uploads[0]))
	}
	var first, second struct {
		Events []map[string]any `json:"events"`
	}
	_ = json.Unmarshal([]byte(bodies[0]), &first)
	_ = json.Unmarshal([]byte(bodies[1]), &second)
	start := first.Events[0]
	if start["event_id"] != "session_1" || start["kind"] != KindSSHStarted || start["origin"] != "online" || start["generation"] != float64(3) {
		t.Fatalf("start on the wire: %v", start)
	}
	end := second.Events[0]
	if len(second.Events) != 3 || end["session_id"] != "session_1" || end["outcome"] != "closed" || end["bytes_in"] != float64(7) || end["bytes_out"] != float64(9) || end["event_id"] == "" || end["event_id"] == "session_1" {
		t.Fatalf("end on the wire: %v (%d events)", end, len(second.Events))
	}
}
