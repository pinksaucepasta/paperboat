package filetransfer

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// delayConn adds a one-way latency to everything read from and written to the connection, in order, with no limit
// on bandwidth: only round trips cost time.
type delayConn struct {
	net.Conn
	delay time.Duration
	in    chan delayed
	out   chan delayed
	buf   []byte
}

type delayed struct {
	at   time.Time
	data []byte
	err  error
}

func newDelayConn(conn net.Conn, delay time.Duration) *delayConn {
	c := &delayConn{Conn: conn, delay: delay, in: make(chan delayed, 4096), out: make(chan delayed, 4096)}
	go func() {
		for {
			buffer := make([]byte, 32<<10)
			n, err := conn.Read(buffer)
			c.in <- delayed{at: time.Now().Add(delay), data: buffer[:n], err: err}
			if err != nil {
				return
			}
		}
	}()
	go func() {
		for item := range c.out {
			time.Sleep(time.Until(item.at))
			if item.err != nil {
				_ = conn.Close()
				return
			}
			if _, err := conn.Write(item.data); err != nil {
				return
			}
		}
	}()
	return c
}

func (c *delayConn) Read(p []byte) (int, error) {
	for len(c.buf) == 0 {
		item := <-c.in
		time.Sleep(time.Until(item.at))
		if item.err != nil && len(item.data) == 0 {
			return 0, item.err
		}
		c.buf = item.data
	}
	n := copy(p, c.buf)
	c.buf = c.buf[n:]
	return n, nil
}

func (c *delayConn) Write(p []byte) (int, error) {
	c.out <- delayed{at: time.Now().Add(c.delay), data: append([]byte(nil), p...)}
	return len(p), nil
}

func (c *delayConn) Close() error {
	c.out <- delayed{at: time.Now().Add(c.delay), err: io.EOF}
	return nil
}

// A 100 ms-each-way link: the old one-MiB-per-acknowledgement send needed a request and answer per MiB (32 MiB = 6.4 s
// of waiting at 200 ms RTT, whatever the bandwidth). The file now goes in one streamed request; asserting the request
// count, not the wall time, keeps the test exact on a loaded machine.
func TestNativeSendIsNotBoundedByRoundTripTimePerChunk(t *testing.T) {
	const size = 32 << 20
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i * 31)
	}
	item := source(data)
	manifest := Manifest{TransferID: "ft_latency", BatchID: "batch_latency", SourceMachineID: "machine_source", DestinationMachineID: "machine_destination", InitiatingUserID: "user_1", Basename: item.Basename, Size: item.Size, SHA256: hex.EncodeToString(item.SHA256[:])}
	var mu sync.Mutex
	var received int64
	var patches atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/file-transfers":
			_ = json.NewEncoder(w).Encode(Batch{BatchID: manifest.BatchID, Transfers: []Manifest{manifest}})
		case r.Method == http.MethodHead:
			mu.Lock()
			w.Header().Set("Upload-Offset", strconv.FormatInt(received, 10))
			mu.Unlock()
		case r.Method == http.MethodPatch:
			patches.Add(1)
			n, err := io.Copy(io.Discard, r.Body)
			if err != nil {
				return
			}
			mu.Lock()
			received += n
			w.Header().Set("Upload-Offset", strconv.FormatInt(received, 10))
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/complete"):
			manifest.CommittedOffset, manifest.State = size, "published"
			_ = json.NewEncoder(w).Encode(map[string]any{"transfer": manifest, "result": map[string]string{"code": "published", "path": "Inbox/data.bin"}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client, err := NewNativeClient(server.URL+"/v1/file-transfers", Auth{Token: "t"}, testBinding(), func(ctx context.Context) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
		if err != nil {
			return nil, err
		}
		return newDelayConn(conn, 100*time.Millisecond), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	started := time.Now()
	if _, err := client.SendBatch(t.Context(), manifest.BatchID, "", []Source{item}); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	if received != size || patches.Load() != 1 {
		t.Fatalf("received=%d in %d requests", received, patches.Load())
	}
	t.Logf("32 MiB over a 200 ms RTT link in %s (%.0f MB/s); the old send spent one round trip per MiB: at least %s", elapsed, size/1e6/elapsed.Seconds(), 32*200*time.Millisecond)
}

type recordingServer struct {
	mu       sync.Mutex
	patchMu  sync.Mutex // one body at a time, like the receiver's per-transfer lock
	received []byte
	patches  int
	deletes  int
}

// streamServer is a receiver that keeps whatever bytes of a PATCH arrive (like the real one) and answers the offset;
// onPatch may block a patch to model a peer that stops reading, and drop it by returning true.
func streamServer(t *testing.T, manifest Manifest, onPatch func(state *recordingServer, patch int, w http.ResponseWriter, r *http.Request) (abort bool)) (*httptest.Server, *recordingServer) {
	t.Helper()
	state := &recordingServer{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/file-transfers":
			_ = json.NewEncoder(w).Encode(Batch{BatchID: manifest.BatchID, Transfers: []Manifest{manifest}})
		case r.Method == http.MethodHead:
			state.mu.Lock()
			w.Header().Set("Upload-Offset", strconv.Itoa(len(state.received)))
			state.mu.Unlock()
		case r.Method == http.MethodPatch:
			state.mu.Lock()
			state.patches++
			patch := state.patches
			state.mu.Unlock()
			buffer := make([]byte, 64<<10)
			if onPatch != nil && onPatch(state, patch, w, r) {
				return
			}
			state.patchMu.Lock()
			defer state.patchMu.Unlock()
			state.mu.Lock()
			stale := r.Header.Get("Upload-Offset") != strconv.Itoa(len(state.received))
			state.mu.Unlock()
			if stale { // like the receiver: an earlier stream's checkpoints may have moved the offset meanwhile
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"code":"offset_conflict","retryable":false}`))
				return
			}
			for {
				n, err := r.Body.Read(buffer)
				state.mu.Lock()
				state.received = append(state.received, buffer[:n]...)
				state.mu.Unlock()
				if err != nil {
					break
				}
			}
			state.mu.Lock()
			w.Header().Set("Upload-Offset", strconv.Itoa(len(state.received)))
			state.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/complete"):
			manifest.CommittedOffset, manifest.State = manifest.Size, "published"
			_ = json.NewEncoder(w).Encode(map[string]any{"transfer": manifest, "result": map[string]string{"code": "published", "path": "Inbox/data.bin"}})
		case r.Method == http.MethodDelete:
			state.mu.Lock()
			state.deletes++
			state.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server, state
}

func nativeClientFor(t *testing.T, server *httptest.Server) *NativeClient {
	t.Helper()
	client, err := NewNativeClient(server.URL+"/v1/file-transfers", Auth{Token: "t"}, testBinding(), func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	})
	if err != nil {
		t.Fatal(err)
	}
	client.retryWait = func(ctx context.Context, _ int) error { return ctx.Err() }
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func latencyManifest(item Source) Manifest {
	return Manifest{TransferID: "ft_stream", BatchID: "batch_stream", SourceMachineID: "machine_source", DestinationMachineID: "machine_destination", InitiatingUserID: "user_1", Basename: item.Basename, Size: item.Size, SHA256: hex.EncodeToString(item.SHA256[:])}
}

func TestNativeSendCutsAStreamThatStopsMovingAndResumesFromWhatArrived(t *testing.T) {
	saved := nativeStallTimeout
	nativeStallTimeout = time.Second
	defer func() { nativeStallTimeout = saved }()
	data := make([]byte, 24<<20)
	for i := range data {
		data[i] = byte(i >> 3)
	}
	item := source(data)
	// The first PATCH: the peer reads nothing and never answers, so the sender's transport stops taking bytes.
	started := time.Now()
	var resumedAfter atomic.Int64
	server, state := streamServer(t, latencyManifest(item), func(_ *recordingServer, patch int, _ http.ResponseWriter, r *http.Request) bool {
		if patch == 2 {
			resumedAfter.Store(int64(time.Since(started)))
		}
		if patch == 1 {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
		}
		return patch == 1
	})
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	client := nativeClientFor(t, server)
	var refreshes atomic.Int32
	client.RefreshAuth = func(context.Context) (Auth, error) { refreshes.Add(1); return Auth{Token: "t"}, nil }
	if _, err := client.SendBatch(ctx, "batch_stream", "", []Source{item}); err != nil {
		t.Fatalf("a stalled stream must be cut and resumed: %v", err)
	}
	if refreshes.Load() != 0 {
		t.Fatalf("a stalled peer made the sender open %d new access sessions", refreshes.Load())
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	// The handler gives up after 5 s on its own: a resume sooner than that is the sender's stall cut.
	if resumedAfter.Load() == 0 || time.Duration(resumedAfter.Load()) > 4*time.Second {
		t.Fatalf("the stalled stream was not cut by the sender: resumed after %s", time.Duration(resumedAfter.Load()))
	}
	if state.patches < 2 || len(state.received) != len(data) {
		t.Fatalf("patches=%d received=%d of %d", state.patches, len(state.received), len(data))
	}
}

type failingReader struct {
	io.ReadSeeker
	after int64
	err   error
}

func (r *failingReader) Read(p []byte) (int, error) {
	position, _ := r.Seek(0, io.SeekCurrent)
	if position >= r.after {
		return 0, r.err
	}
	return r.ReadSeeker.Read(p[:min(int64(len(p)), r.after-position)])
}

func TestNativeSendDoesNotRetryASourceThatFailsMidStream(t *testing.T) {
	data := make([]byte, 4<<20)
	item := source(data)
	item.Reader = &failingReader{ReadSeeker: item.Reader.(io.ReadSeeker), after: 1 << 20, err: io.ErrClosedPipe}
	server, state := streamServer(t, latencyManifest(item), nil)
	_, err := nativeClientFor(t, server).SendBatch(t.Context(), "batch_stream", "", []Source{item})
	var failure *nativeSourceFailure
	if !errors.As(err, &failure) {
		t.Fatalf("err=%v", err)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.patches != 1 {
		t.Fatalf("a failing source was retried: %d patches", state.patches)
	}
}

func TestNativeSendProgressFollowsTheStreamAndEndsAtTheConfirmedOffset(t *testing.T) {
	data := make([]byte, 5<<20)
	item := source(data)
	server, _ := streamServer(t, latencyManifest(item), nil)
	client := nativeClientFor(t, server)
	var mu sync.Mutex
	var reports []int64
	client.Committed = func(_ string, offset int64) {
		mu.Lock()
		reports = append(reports, offset)
		mu.Unlock()
	}
	if _, err := client.SendBatch(t.Context(), "batch_stream", "", []Source{item}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reports) < 5 || reports[len(reports)-1] != int64(len(data)) {
		t.Fatalf("progress reports: %v", reports)
	}
	for i := 1; i < len(reports); i++ {
		if reports[i] < reports[i-1] {
			t.Fatalf("progress went backwards: %v", reports)
		}
	}
}

// A cut stream's last checkpoint can still commit after the sender's next HEAD, so the next PATCH meets an offset the
// receiver has already passed: the sender asks again and carries on from there instead of failing the send.
func TestNativeSendAsksAgainWhenTheReceiverIsPastItsOffset(t *testing.T) {
	data := make([]byte, 3<<20)
	for i := range data {
		data[i] = byte(i * 5)
	}
	item := source(data)
	server, state := streamServer(t, latencyManifest(item), func(state *recordingServer, patch int, w http.ResponseWriter, _ *http.Request) bool {
		if patch != 1 {
			return false
		}
		state.mu.Lock()
		state.received = append(state.received, data[:1<<20]...)
		state.mu.Unlock()
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"code":"offset_conflict","retryable":false}`))
		return true
	})
	if _, err := nativeClientFor(t, server).SendBatch(t.Context(), "batch_stream", "", []Source{item}); err != nil {
		t.Fatal(err)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if !bytes.Equal(state.received, data) || state.patches != 2 {
		t.Fatalf("received %d of %d bytes in %d requests", len(state.received), len(data), state.patches)
	}
}

// keepOneMiBThenCut makes a PATCH commit the first MiB of its body and then lose the connection, the way a receiver
// does when a stream breaks after a checkpoint.
func keepOneMiBThenCut(delay time.Duration) func(*recordingServer, int, http.ResponseWriter, *http.Request) bool {
	return func(state *recordingServer, _ int, w http.ResponseWriter, r *http.Request) bool {
		chunk := make([]byte, 1<<20)
		n, _ := io.ReadFull(r.Body, chunk)
		state.mu.Lock()
		state.received = append(state.received, chunk[:n]...)
		state.mu.Unlock()
		if n < len(chunk) {
			return false // never reached: the caller only uses this for streams longer than a MiB
		}
		time.Sleep(delay)
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			_ = conn.Close()
		}
		return true
	}
}

func TestNativeSendRestartsTheRecoveryWindowWhenACutStreamHadCommittedCheckpoints(t *testing.T) {
	saved, savedSegment := nativeRecoveryWindow, nativeSegmentBytes
	nativeRecoveryWindow, nativeSegmentBytes = 400*time.Millisecond, 2<<20
	defer func() { nativeRecoveryWindow, nativeSegmentBytes = saved, savedSegment }()
	data := make([]byte, 8<<20)
	for i := range data {
		data[i] = byte(i * 11)
	}
	item := source(data)
	cut := func(state *recordingServer, patch int, w http.ResponseWriter, r *http.Request) bool {
		if int64(len(data))-int64(patch-1)*(1<<20) <= 1<<20 { // the last MiB is a whole segment: answer it
			return false
		}
		return keepOneMiBThenCut(150*time.Millisecond)(state, patch, w, r)
	}
	server, state := streamServer(t, latencyManifest(item), cut)
	// 7 cut streams of 150 ms each outlast the 400 ms window: only HEAD showing a checkpoint after each cut keeps it open.
	if _, err := nativeClientFor(t, server).SendBatch(t.Context(), "batch_stream", "", []Source{item}); err != nil {
		t.Fatalf("a send whose every stream committed a checkpoint before it was cut must finish: %v", err)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if !bytes.Equal(state.received, data) {
		t.Fatalf("received %d of %d bytes", len(state.received), len(data))
	}
}

func TestNativeSendRetriesWhileEveryReceiverSlotIsTaken(t *testing.T) {
	data := make([]byte, 1<<20)
	item := source(data)
	server, state := streamServer(t, latencyManifest(item), func(state *recordingServer, patch int, w http.ResponseWriter, _ *http.Request) bool {
		if patch != 1 {
			return false
		}
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"code":"resource_limit","retryable":true}`))
		return true
	})
	if _, err := nativeClientFor(t, server).SendBatch(t.Context(), "batch_stream", "", []Source{item}); err != nil {
		t.Fatal(err)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.received) != len(data) || state.patches != 2 {
		t.Fatalf("received %d bytes in %d requests", len(state.received), state.patches)
	}
}

func TestNativeSendProgressNeverGoesBackwardsAfterACut(t *testing.T) {
	savedSegment := nativeSegmentBytes
	nativeSegmentBytes = 4 << 20
	defer func() { nativeSegmentBytes = savedSegment }()
	data := make([]byte, 6<<20)
	item := source(data)
	server, _ := streamServer(t, latencyManifest(item), func(state *recordingServer, patch int, w http.ResponseWriter, r *http.Request) bool {
		if patch == 1 {
			return keepOneMiBThenCut(0)(state, patch, w, r)
		}
		return false
	})
	client := nativeClientFor(t, server)
	var mu sync.Mutex
	var reports []int64
	client.Committed = func(_ string, offset int64) {
		mu.Lock()
		reports = append(reports, offset)
		mu.Unlock()
	}
	if _, err := client.SendBatch(t.Context(), "batch_stream", "", []Source{item}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	for i := 1; i < len(reports); i++ {
		if reports[i] <= reports[i-1] {
			t.Fatalf("progress is not increasing: %v", reports)
		}
	}
	if len(reports) < 6 || reports[len(reports)-1] != int64(len(data)) {
		t.Fatalf("progress ends at %d: %v", reports[len(reports)-1], reports)
	}
}

func TestNativeSendFailsAtOnceOnAnOffsetConflictThatHeadDoesNotExplain(t *testing.T) {
	item := source(make([]byte, 1<<20))
	server, state := streamServer(t, latencyManifest(item), func(_ *recordingServer, _ int, w http.ResponseWriter, _ *http.Request) bool {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"code":"offset_conflict","retryable":false}`))
		return true
	})
	started := time.Now()
	_, err := nativeClientFor(t, server).SendBatch(t.Context(), "batch_stream", "", []Source{item})
	var remote *Error
	if !errors.As(err, &remote) || remote.Code != "offset_conflict" {
		t.Fatalf("err=%v", err)
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.patches != 1 || time.Since(started) > 5*time.Second {
		t.Fatalf("a conflict HEAD does not explain was retried: %d requests in %s", state.patches, time.Since(started))
	}
}

func TestNativeSendKeepsTheBatchResumableWhenTheReceiverStaysBusy(t *testing.T) {
	saved := nativeRecoveryWindow
	nativeRecoveryWindow = 300 * time.Millisecond
	defer func() { nativeRecoveryWindow = saved }()
	item := source(make([]byte, 1<<20))
	server, state := streamServer(t, latencyManifest(item), func(_ *recordingServer, _ int, w http.ResponseWriter, _ *http.Request) bool {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"code":"resource_limit","retryable":true}`))
		return true
	})
	client := nativeClientFor(t, server)
	var waiting atomic.Int32
	client.Waiting = func(string) { waiting.Add(1) }
	client.retryWait = func(ctx context.Context, _ int) error { time.Sleep(50 * time.Millisecond); return ctx.Err() }
	_, err := client.SendBatch(t.Context(), "batch_stream", "", []Source{item})
	var resume *ResumeRequiredError
	state.mu.Lock()
	defer state.mu.Unlock()
	if !errors.As(err, &resume) || state.deletes != 0 || waiting.Load() == 0 {
		t.Fatalf("err=%v deletes=%d waiting messages=%d", err, state.deletes, waiting.Load())
	}
}
