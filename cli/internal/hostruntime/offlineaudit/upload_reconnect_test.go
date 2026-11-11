package offlineaudit

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type scriptedTransport struct {
	next  http.RoundTripper
	calls atomic.Int32
	// unreachable is how many first calls fail before any connection exists.
	unreachable int32
	mu          sync.Mutex
	at          []time.Time
}

func (t *scriptedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.mu.Lock()
	t.at = append(t.at, time.Now())
	t.mu.Unlock()
	if t.calls.Add(1) <= t.unreachable {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("no route to host")}
	}
	return t.next.RoundTrip(request)
}

func shortenUnreachable(t *testing.T, first, max time.Duration) {
	t.Helper()
	previousFirst, previousMax := unreachableRetry, unreachableMax
	unreachableRetry, unreachableMax = first, max
	t.Cleanup(func() { unreachableRetry, unreachableMax = previousFirst, previousMax })
}

func reconnectUploader(t *testing.T, unreachable int32, status int) (*Uploader, *Queue, *scriptedTransport) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = w.Write([]byte(`{"data":{"accepted":1,"duplicates":0}}`))
		}
	}))
	t.Cleanup(server.Close)
	queue, err := Open(t.TempDir()+"/audit.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := queue.Record(Event{EventID: "session_1", Kind: KindSSHStarted, PeerEndpointID: "cli_1", PeerAccountID: "acct", Origin: OriginOffline, Generation: 1}); err != nil {
		t.Fatal(err)
	}
	transport := &scriptedTransport{next: server.Client().Transport, unreachable: unreachable}
	uploader := &Uploader{Queue: queue, ServerURL: server.URL, MachineID: "machine_1", Client: &http.Client{Transport: transport}, Tokens: fixedTokens{}, Proofs: &recordingProofs{},
		OperationID: func() (string, error) { return "operation_1", nil }, MinInterval: UploadInterval}
	return uploader, queue, transport
}

// Sessions started while Paperboat was unreachable are audited once it is back: an upload that never opened a
// connection retries within seconds, not after the doubling backoff or the 30 s spacing (measured on a macOS host: the
// backlog went up 5 min after the network came back).
func TestUploaderFlushesSoonAfterTheServerBecomesReachable(t *testing.T) {
	shortenUnreachable(t, 20*time.Millisecond, 20*time.Millisecond)
	uploader, queue, transport := reconnectUploader(t, 3, http.StatusOK)
	go uploader.Run(t.Context())
	deadline := time.Now().Add(3 * time.Second)
	for queue.Len() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d event(s) still queued after %d upload attempts, want them flushed within seconds of the server being reachable", queue.Len(), transport.calls.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := transport.calls.Load(); got != 4 {
		t.Fatalf("attempts = %d, want 3 unreachable then 1 delivered", got)
	}
}

// A server that answers with a failure keeps the doubling backoff: it is not asked again every few seconds.
func TestUploaderStillBacksOffWhenTheServerAnswersWithAFailure(t *testing.T) {
	shortenUnreachable(t, 20*time.Millisecond, 20*time.Millisecond)
	uploader, queue, transport := reconnectUploader(t, 0, http.StatusServiceUnavailable)
	go uploader.Run(t.Context())
	time.Sleep(700 * time.Millisecond)
	if got := transport.calls.Load(); got != 1 || queue.Len() != 1 {
		t.Fatalf("attempts = %d, queued = %d, want one attempt and the event kept (the retry waits at least firstRetry)", got, queue.Len())
	}
}

// A machine offline for hours (a laptop on a plane) must not retry every few seconds forever: the waits double up to the
// cap and stay there. The waits are recorded as scheduled (a timer hook that fires at once), so the test does not depend on
// the host's scheduler.
func TestUploaderSettlesAtTheCapDuringALongOutage(t *testing.T) {
	const first, limit = 10 * time.Millisecond, 80 * time.Millisecond
	shortenUnreachable(t, first, limit)
	var mu sync.Mutex
	var waits []time.Duration
	enough := make(chan struct{})
	previous := retryTimer
	retryTimer = func(d time.Duration) <-chan time.Time {
		mu.Lock()
		waits = append(waits, d)
		if len(waits) == 20 {
			close(enough)
		}
		count := len(waits)
		mu.Unlock()
		if count >= 20 { // park the loop once enough waits were seen
			return nil
		}
		fired := make(chan time.Time, 1)
		fired <- time.Now()
		return fired
	}
	t.Cleanup(func() { retryTimer = previous })
	uploader, _, _ := reconnectUploader(t, 1<<30, http.StatusOK)
	go uploader.Run(t.Context())
	select {
	case <-enough:
	case <-time.After(5 * time.Second):
		t.Fatal("the uploader did not keep retrying")
	}
	mu.Lock()
	defer mu.Unlock()
	for i, wait := range waits { // jitter spreads each wait over [d/2, 3d/2); d doubles from first up to the cap
		d := min(first<<min(i, 10), limit)
		if wait < d/2 || wait >= 3*d/2 {
			t.Fatalf("wait %d = %v, want within [%v, %v) (doubling from %v up to the %v cap)", i+1, wait, d/2, 3*d/2, first, limit)
		}
	}
	if last := waits[len(waits)-1]; last < limit/2 {
		t.Fatalf("the long outage ended at a %v wait, want it settled at the %v cap (not hammering)", last, limit)
	}
}

// The control plane being reachable again ends the wait at once, even at the cap.
func TestUploaderWakesWhenTheControlPlaneIsReachableAgain(t *testing.T) {
	shortenUnreachable(t, time.Hour, time.Hour)
	uploader, queue, transport := reconnectUploader(t, 2, http.StatusOK)
	wake := make(chan struct{}, 1)
	uploader.Wake = wake
	go uploader.Run(t.Context())
	deadline := time.Now().Add(2 * time.Second)
	for transport.calls.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	wake <- struct{}{} // second failure, now waiting an hour
	for transport.calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	wake <- struct{}{}
	for queue.Len() != 0 {
		if time.Now().After(deadline.Add(2 * time.Second)) {
			t.Fatalf("%d event(s) still queued; the wake did not end the hour-long wait", queue.Len())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
