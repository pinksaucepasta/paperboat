package offlineaudit

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

type scriptedTransport struct {
	next  http.RoundTripper
	calls atomic.Int32
	// unreachable is how many first calls fail before any connection exists.
	unreachable int32
}

func (t *scriptedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if t.calls.Add(1) <= t.unreachable {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("no route to host")}
	}
	return t.next.RoundTrip(request)
}

func reconnectUploader(t *testing.T, unreachable int32, status int) (*Uploader, *Queue, *scriptedTransport) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		if status == http.StatusOK {
			_, _ = w.Write([]byte(`{"accepted":1,"duplicates":0}`))
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
	previous := unreachableRetry
	unreachableRetry = 20 * time.Millisecond
	t.Cleanup(func() { unreachableRetry = previous })
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
	previous := unreachableRetry
	unreachableRetry = 20 * time.Millisecond
	t.Cleanup(func() { unreachableRetry = previous })
	uploader, queue, transport := reconnectUploader(t, 0, http.StatusServiceUnavailable)
	go uploader.Run(t.Context())
	time.Sleep(700 * time.Millisecond)
	if got := transport.calls.Load(); got != 1 || queue.Len() != 1 {
		t.Fatalf("attempts = %d, queued = %d, want one attempt and the event kept (the retry waits at least firstRetry)", got, queue.Len())
	}
}
