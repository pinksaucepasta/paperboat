package filetransfer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// busyJSONServer is streamServer with a 429-injection switch on create or on
// complete. PATCH/HEAD/DELETE shapes match streamServer
// (native_latency_test.go:161-218) so SendBatch validation still applies.
type busyJSONServer struct {
	manifest      Manifest
	mu            sync.Mutex
	received      []byte
	creates       int
	completes     int
	deletes       int
	failCreates   int // first N creates answer 429 resource_limit
	failCompletes int // first N completes answer 429 resource_limit (<=0 = always when alwaysFail true)
	alwaysFail    bool
	waiting       *atomic.Int32
}

func (s *busyJSONServer) handler(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/file-transfers":
		s.mu.Lock()
		s.creates++
		n := s.creates
		s.mu.Unlock()
		if n <= s.failCreates || (s.alwaysFail && s.failCreates < 0) {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"code":"resource_limit","retryable":true}`))
			return
		}
		_ = json.NewEncoder(w).Encode(Batch{BatchID: s.manifest.BatchID, Transfers: []Manifest{s.manifest}})
	case r.Method == http.MethodHead:
		s.mu.Lock()
		w.Header().Set("Upload-Offset", strconv.Itoa(len(s.received)))
		s.mu.Unlock()
	case r.Method == http.MethodPatch:
		s.mu.Lock()
		stale := r.Header.Get("Upload-Offset") != strconv.Itoa(len(s.received))
		s.mu.Unlock()
		if stale {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"code":"offset_conflict","retryable":false}`))
			return
		}
		buf := make([]byte, 64<<10)
		for {
			n, err := r.Body.Read(buf)
			s.mu.Lock()
			s.received = append(s.received, buf[:n]...)
			s.mu.Unlock()
			if err != nil {
				break
			}
		}
		s.mu.Lock()
		w.Header().Set("Upload-Offset", strconv.Itoa(len(s.received)))
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/complete"):
		s.mu.Lock()
		s.completes++
		n := s.completes
		s.mu.Unlock()
		if n <= s.failCompletes || (s.alwaysFail && s.failCompletes < 0) {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"code":"resource_limit","retryable":true}`))
			return
		}
		done := s.manifest
		s.mu.Lock()
		done.CommittedOffset = int64(len(s.received))
		s.mu.Unlock()
		done.State = "published"
		_ = json.NewEncoder(w).Encode(map[string]any{"transfer": done, "result": map[string]string{"code": "published", "path": "Inbox/data.bin"}})
	case r.Method == http.MethodDelete:
		s.mu.Lock()
		s.deletes++
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func busyNativeClient(t *testing.T, server *httptest.Server, state *busyJSONServer) *NativeClient {
	t.Helper()
	client := nativeClientFor(t, server)
	client.Waiting = func(string) { state.waiting.Add(1) }
	return client
}

func TestNativeCreateRetriesWhileReceiverBusy(t *testing.T) {
	data := bytes.Repeat([]byte("c"), 1<<20)
	item := source(data)
	state := &busyJSONServer{manifest: latencyManifest(item), failCreates: 1, waiting: &atomic.Int32{}}
	server := httptest.NewServer(http.HandlerFunc(state.handler))
	defer server.Close()
	client := busyNativeClient(t, server, state)
	batch, err := client.SendBatch(t.Context(), "batch_stream", "", []Source{item})
	state.mu.Lock()
	defer state.mu.Unlock()
	if err != nil || len(batch.Transfers) != 1 || batch.Transfers[0].TransferID != "ft_stream" {
		t.Fatalf("create 429 then success: err=%v batch=%#v", err, batch)
	}
	if state.creates != 2 || len(state.received) != len(data) || state.waiting.Load() == 0 {
		t.Fatalf("creates=%d received=%d of %d waiting=%d", state.creates, len(state.received), len(data), state.waiting.Load())
	}
}

func TestNativeCreateStaysResumableWhenReceiverStaysBusy(t *testing.T) {
	saved := nativeRecoveryWindow
	nativeRecoveryWindow = 300 * time.Millisecond
	defer func() { nativeRecoveryWindow = saved }()
	data := bytes.Repeat([]byte("c"), 1<<20)
	item := source(data)
	state := &busyJSONServer{manifest: latencyManifest(item), failCreates: -1, alwaysFail: true, waiting: &atomic.Int32{}}
	server := httptest.NewServer(http.HandlerFunc(state.handler))
	defer server.Close()
	client := busyNativeClient(t, server, state)
	client.retryWait = func(ctx context.Context, _ int) error { time.Sleep(50 * time.Millisecond); return ctx.Err() }
	_, err := client.SendBatch(t.Context(), "batch_stream", "", []Source{item})
	var resume *ResumeRequiredError
	state.mu.Lock()
	defer state.mu.Unlock()
	if !errors.As(err, &resume) || resume.BatchID != "batch_stream" || state.deletes != 0 || state.waiting.Load() == 0 {
		t.Fatalf("err=%v deletes=%d waiting=%d", err, state.deletes, state.waiting.Load())
	}
}

func TestNativeCompleteRetriesWhileReceiverBusy(t *testing.T) {
	data := bytes.Repeat([]byte("d"), 1<<20)
	item := source(data)
	state := &busyJSONServer{manifest: latencyManifest(item), failCompletes: 1, waiting: &atomic.Int32{}}
	server := httptest.NewServer(http.HandlerFunc(state.handler))
	defer server.Close()
	client := busyNativeClient(t, server, state)
	batch, err := client.SendBatch(t.Context(), "batch_stream", "", []Source{item})
	state.mu.Lock()
	defer state.mu.Unlock()
	if err != nil || len(batch.Transfers) != 1 || batch.Transfers[0].State != "published" {
		t.Fatalf("complete 429 then success: err=%v batch=%#v", err, batch)
	}
	if state.completes != 2 || state.waiting.Load() == 0 {
		t.Fatalf("completes=%d waiting=%d", state.completes, state.waiting.Load())
	}
}

func TestNativeCompleteStaysResumableWhenReceiverStaysBusy(t *testing.T) {
	saved := nativeRecoveryWindow
	nativeRecoveryWindow = 300 * time.Millisecond
	defer func() { nativeRecoveryWindow = saved }()
	data := bytes.Repeat([]byte("d"), 1<<20)
	item := source(data)
	state := &busyJSONServer{manifest: latencyManifest(item), failCompletes: -1, alwaysFail: true, waiting: &atomic.Int32{}}
	server := httptest.NewServer(http.HandlerFunc(state.handler))
	defer server.Close()
	client := busyNativeClient(t, server, state)
	client.retryWait = func(ctx context.Context, _ int) error { time.Sleep(50 * time.Millisecond); return ctx.Err() }
	_, err := client.SendBatch(t.Context(), "batch_stream", "", []Source{item})
	var resume *ResumeRequiredError
	state.mu.Lock()
	defer state.mu.Unlock()
	if !errors.As(err, &resume) || resume.BatchID != "batch_stream" || state.deletes != 0 || state.waiting.Load() == 0 {
		t.Fatalf("err=%v deletes=%d waiting=%d", err, state.deletes, state.waiting.Load())
	}
}
