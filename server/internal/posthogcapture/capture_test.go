package posthogcapture

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNilClientIsOff(t *testing.T) {
	var c *Client
	if New(Config{}) != nil || c.Capture(Event{Name: "x", Distinct: "a"}) || c.CaptureDaily(Event{Name: "x", Distinct: "a"}) {
		t.Fatal("an unconfigured client must do nothing")
	}
}

func TestQueueIsBoundedAndNeverBlocks(t *testing.T) {
	c := New(Config{Host: "https://ph.example.test", Key: "k"})
	done := make(chan int)
	go func() {
		n := 0
		for range queueSize + 50 {
			if c.Capture(Event{Name: "e", Distinct: "acct"}) {
				n++
			}
		}
		done <- n
	}()
	select {
	case n := <-done:
		if n != queueSize {
			t.Fatalf("accepted %d, want %d", n, queueSize)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Capture blocked on a full queue")
	}
}

func TestOnlyAllowedLabelsAreSent(t *testing.T) {
	c := New(Config{Host: "https://ph.example.test", Key: "k"})
	c.Capture(Event{Name: "feature_used", Distinct: "acct", Props: map[string]string{
		"feature": "terminal", "email": "a@example.test", "machine_name": "laptop", "command": "ls", "ip": "1.2.3.4"}})
	body, err := c.body([]Event{<-c.queue})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Batch []struct {
			Properties map[string]any `json:"properties"`
		} `json:"batch"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	p := got.Batch[0].Properties
	for _, bad := range []string{"email", "machine_name", "command", "ip"} {
		if _, ok := p[bad]; ok {
			t.Errorf("property %q must never be sent", bad)
		}
	}
	if p["feature"] != "terminal" || p["app"] != "server" || p["$geoip_disable"] != true {
		t.Errorf("properties = %v", p)
	}
}

func TestCaptureDailyOncePerDay(t *testing.T) {
	now := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	c := New(Config{Host: "https://ph.example.test", Key: "k", Now: func() time.Time { return now }})
	e := Event{Name: "feature_used", Distinct: "acct", Props: map[string]string{"feature": "terminal"}}
	if !c.CaptureDaily(e) || c.CaptureDaily(e) {
		t.Fatal("the second use on the same day must be dropped")
	}
	other := e
	other.Props = map[string]string{"feature": "exec"}
	if !c.CaptureDaily(other) {
		t.Fatal("another feature is its own event")
	}
	now = now.Add(24 * time.Hour)
	if !c.CaptureDaily(e) {
		t.Fatal("the next day sends again")
	}
}

func TestRunBatchesAndDropsOnFailure(t *testing.T) {
	var mu sync.Mutex
	var sizes []int
	status := http.StatusInternalServerError
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var in struct {
			Key   string  `json:"api_key"`
			Batch []Event `json:"batch"`
		}
		_ = json.Unmarshal(raw, &in)
		mu.Lock()
		sizes = append(sizes, len(in.Batch))
		mu.Unlock()
		if !strings.HasSuffix(r.URL.Path, "/batch/") || in.Key != "k" {
			t.Errorf("unexpected request %s key %q", r.URL.Path, in.Key)
		}
		w.WriteHeader(status)
	}))
	defer srv.Close()
	c := New(Config{Host: srv.URL, Key: "k"})
	for range batchSize + 5 {
		c.Capture(Event{Name: "e", Distinct: "acct"})
	}
	// The context ends before Run reads the queue, the schedule a loaded host can produce at any point: the
	// shutdown flush alone must still split the queued events into batches of at most batchSize.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = c.Run(ctx)
	mu.Lock()
	defer mu.Unlock()
	if len(sizes) != 2 || sizes[0] != batchSize || sizes[1] != 5 {
		t.Fatalf("batches = %v, want [%d 5] even though every delivery failed", sizes, batchSize)
	}
}
