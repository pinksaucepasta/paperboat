package filetransfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"github.com/pinksaucepasta/paperboat/shared/reattach"
)

// F14-02 on a hosted Windows runner: the update cut the connection once, the send recovered and kept committing at
// ~0.5 MiB/s, and failed with operation_timeout 32 min after the cut at 98%: the recovery deadline stayed on every
// later request. A committed chunk ends the window, so a slow transfer that keeps committing finishes.
func TestASlowTransferThatKeepsCommittingOutlivesTheRecoveryWindow(t *testing.T) {
	saved := nativeRecoveryWindow
	nativeRecoveryWindow = 300 * time.Millisecond
	defer func() { nativeRecoveryWindow = saved }()
	data := bytes.Repeat([]byte("slow"), 6*protocol.FileTransferChunkBytes/4)
	item := source(data)
	manifest := Manifest{TransferID: "ft_slow", BatchID: "batch_slow", SourceMachineID: "machine_source", DestinationMachineID: "machine_destination", InitiatingUserID: "user_1", Basename: item.Basename, Size: item.Size, SHA256: hex.EncodeToString(item.SHA256[:])}
	var mu sync.Mutex
	var content []byte
	dropped := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/file-transfers":
			_ = json.NewEncoder(w).Encode(Batch{BatchID: manifest.BatchID, Transfers: []Manifest{manifest}})
		case r.Method == http.MethodHead:
			w.Header().Set("Upload-Offset", strconv.Itoa(len(content)))
			w.WriteHeader(204)
		case r.Method == http.MethodPatch:
			chunk, err := io.ReadAll(r.Body)
			digest := sha256.Sum256(chunk)
			if err != nil || r.Header.Get("Upload-Offset") != strconv.Itoa(len(content)) || r.Header.Get("Upload-Digest") != "sha256="+hex.EncodeToString(digest[:]) {
				w.WriteHeader(400)
				return
			}
			if !dropped { // the update: the first chunk's connection is cut before it commits
				dropped = true
				connection, _, _ := w.(http.Hijacker).Hijack()
				connection.Close()
				return
			}
			time.Sleep(100 * time.Millisecond) // a slow path: the remaining chunks take longer than the window
			content = append(content, chunk...)
			w.Header().Set("Upload-Offset", strconv.Itoa(len(content)))
			w.WriteHeader(204)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/complete"):
			manifest.CommittedOffset, manifest.State = int64(len(content)), "published"
			_ = json.NewEncoder(w).Encode(map[string]any{"transfer": manifest, "result": map[string]string{"code": "published", "path": "Inbox/data.bin"}})
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	lease := &nativeTestLease{address: server.Listener.Addr().String(), expires: time.Now().Add(time.Minute)}
	client, err := NewNativeClient(server.URL+"/v1/file-transfers", Auth{Token: "t", ExpiresAt: lease.expires}, testBinding(), lease.OpenTransferStream)
	if err != nil {
		t.Fatal(err)
	}
	client.retryWait = func(ctx context.Context, _ int) error { return ctx.Err() }
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := client.SendBatch(ctx, manifest.BatchID, "", []Source{item}); err != nil || !bytes.Equal(content, data) {
		t.Fatalf("a slow transfer that kept committing failed after the recovery window: %v (%d of %d bytes)", err, len(content), len(data))
	}
}

// The opposite case: a send that never commits a chunk ends one window after its FIRST failure; the many retries in
// between must not keep extending the deadline.
func TestASendWithoutProgressEndsOneWindowAfterItsFirstFailure(t *testing.T) {
	saved := nativeRecoveryWindow
	// Long enough that a Windows runner (coarse timers, slow connection setup) still fits 3 attempts inside it.
	nativeRecoveryWindow = 1500 * time.Millisecond
	defer func() { nativeRecoveryWindow = saved }()
	item := source(bytes.Repeat([]byte("stuck"), protocol.FileTransferChunkBytes/5))
	manifest := Manifest{TransferID: "ft_stuck", BatchID: "batch_stuck", SourceMachineID: "machine_source", DestinationMachineID: "machine_destination", InitiatingUserID: "user_1", Basename: item.Basename, Size: item.Size, SHA256: hex.EncodeToString(item.SHA256[:])}
	var mu sync.Mutex
	var cuts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/file-transfers":
			_ = json.NewEncoder(w).Encode(Batch{BatchID: manifest.BatchID, Transfers: []Manifest{manifest}})
		case r.Method == http.MethodHead:
			w.Header().Set("Upload-Offset", "0")
			w.WriteHeader(204)
		case r.Method == http.MethodPatch: // every chunk's connection is cut before it commits
			cuts++
			connection, _, _ := w.(http.Hijacker).Hijack()
			connection.Close()
		default:
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	lease := &nativeTestLease{address: server.Listener.Addr().String(), expires: time.Now().Add(time.Minute)}
	client, err := NewNativeClient(server.URL+"/v1/file-transfers", Auth{Token: "t", ExpiresAt: lease.expires}, testBinding(), lease.OpenTransferStream)
	if err != nil {
		t.Fatal(err)
	}
	client.retryWait = func(ctx context.Context, _ int) error { return reattach.Wait(ctx, 20*time.Millisecond) }
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	started := time.Now()
	_, err = client.SendBatch(ctx, manifest.BatchID, "", []Source{item})
	elapsed := time.Since(started)
	mu.Lock()
	defer mu.Unlock()
	if err == nil || ctx.Err() != nil {
		t.Fatalf("a send without progress did not fail on its own: %v (caller context: %v)", err, ctx.Err())
	}
	if cuts < 3 {
		t.Fatalf("only %d failed attempts: the retries the window must bound did not happen", cuts)
	}
	if elapsed < nativeRecoveryWindow || elapsed > 4*nativeRecoveryWindow {
		t.Fatalf("send ended %v after its first failure, want about the %v window", elapsed, nativeRecoveryWindow)
	}
}
