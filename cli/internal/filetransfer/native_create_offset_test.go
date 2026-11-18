package filetransfer

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The create response already carries the receiver's durable offset, so a
// fresh batch uploads with no HEAD: one peer round trip saved per file.
func TestCreateOffsetSkipsHeadOnFreshBatch(t *testing.T) {
	data := []byte("fresh batch without a head request")
	item := source(data)
	manifest := Manifest{TransferID: "ft_fresh", BatchID: "batch_fresh", SourceMachineID: "machine_source", DestinationMachineID: "machine_destination", InitiatingUserID: "user_1", Basename: item.Basename, Size: item.Size, SHA256: sourceDigest(data), CommittedOffset: 0, State: "created"}
	var mu sync.Mutex
	var content []byte
	var heads, patches atomic.Int32
	var patchOffset atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/file-transfers":
			_ = json.NewEncoder(w).Encode(Batch{BatchID: manifest.BatchID, Transfers: []Manifest{manifest}})
		case r.Method == http.MethodHead:
			heads.Add(1)
			w.Header().Set("Upload-Offset", strconv.Itoa(len(content)))
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPatch:
			patches.Add(1)
			offset, _ := strconv.ParseInt(r.Header.Get("Upload-Offset"), 10, 64)
			patchOffset.Store(offset)
			chunk, err := io.ReadAll(r.Body)
			if err != nil || offset != int64(len(content)) {
				w.WriteHeader(400)
				return
			}
			content = append(content, chunk...)
			w.Header().Set("Upload-Offset", strconv.Itoa(len(content)))
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/complete"):
			manifest.CommittedOffset = int64(len(content))
			manifest.State = "published"
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
	defer client.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := client.SendBatch(ctx, manifest.BatchID, "", []Source{item}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if heads.Load() != 0 {
		t.Fatalf("fresh batch used %d HEAD requests", heads.Load())
	}
	if patches.Load() != 1 || patchOffset.Load() != 0 {
		t.Fatalf("patches=%d first offset=%d", patches.Load(), patchOffset.Load())
	}
	if !bytes.Equal(content, data) {
		t.Fatal("receiver content differs")
	}
}

// A replayed create (same batch retried after a lost answer) may carry the
// journal's original outcome instead of the live offset, so it starts with
// exactly one HEAD and resumes from the receiver's authoritative offset.
func TestReplayedCreateHeadsOnceThenResumes(t *testing.T) {
	data := []byte("replayed batch resumes from the committed prefix")
	item := source(data)
	prefix := len(data) / 2
	manifest := Manifest{TransferID: "ft_replay", BatchID: "batch_replay", SourceMachineID: "machine_source", DestinationMachineID: "machine_destination", InitiatingUserID: "user_1", Basename: item.Basename, Size: item.Size, SHA256: sourceDigest(data), CommittedOffset: int64(prefix), State: "created"}
	var mu sync.Mutex
	content := append([]byte(nil), data[:prefix]...)
	var heads, patches atomic.Int32
	var patchOffsets []int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/file-transfers":
			w.Header().Set("X-Paperboat-Replayed", "true")
			_ = json.NewEncoder(w).Encode(Batch{BatchID: manifest.BatchID, Transfers: []Manifest{manifest}})
		case r.Method == http.MethodHead:
			heads.Add(1)
			w.Header().Set("Upload-Offset", strconv.Itoa(len(content)))
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPatch:
			patches.Add(1)
			offset, _ := strconv.ParseInt(r.Header.Get("Upload-Offset"), 10, 64)
			patchOffsets = append(patchOffsets, offset)
			chunk, err := io.ReadAll(r.Body)
			if err != nil || offset != int64(len(content)) {
				w.WriteHeader(400)
				return
			}
			content = append(content, chunk...)
			w.Header().Set("Upload-Offset", strconv.Itoa(len(content)))
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/complete"):
			manifest.CommittedOffset = int64(len(content))
			manifest.State = "published"
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
	defer client.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := client.SendBatch(ctx, manifest.BatchID, "", []Source{item}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if heads.Load() != 1 {
		t.Fatalf("replayed batch used %d HEAD requests, want exactly one", heads.Load())
	}
	if len(patchOffsets) != 1 || patchOffsets[0] != int64(prefix) {
		t.Fatalf("patch offsets=%v, want one resume at %d", patchOffsets, prefix)
	}
	if !bytes.Equal(content, data) {
		t.Fatal("receiver content differs")
	}
}

// A stale create offset (a non-replayed answer older than the receiver's
// state) is corrected by the existing offset_conflict path: one refused
// PATCH, one HEAD, then the resume. Nothing is delivered twice.
func TestStaleCreateOffsetFallsBackToHead(t *testing.T) {
	data := []byte("stale create offset corrected by one head")
	item := source(data)
	prefix := len(data) / 2
	manifest := Manifest{TransferID: "ft_stale", BatchID: "batch_stale", SourceMachineID: "machine_source", DestinationMachineID: "machine_destination", InitiatingUserID: "user_1", Basename: item.Basename, Size: item.Size, SHA256: sourceDigest(data), CommittedOffset: 0, State: "created"}
	var mu sync.Mutex
	content := append([]byte(nil), data[:prefix]...)
	var heads, patches, conflicts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/file-transfers":
			_ = json.NewEncoder(w).Encode(Batch{BatchID: manifest.BatchID, Transfers: []Manifest{manifest}})
		case r.Method == http.MethodHead:
			heads.Add(1)
			w.Header().Set("Upload-Offset", strconv.Itoa(len(content)))
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPatch:
			patches.Add(1)
			offset, _ := strconv.ParseInt(r.Header.Get("Upload-Offset"), 10, 64)
			chunk, err := io.ReadAll(r.Body)
			if err != nil || offset != int64(len(content)) {
				conflicts.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"code":"offset_conflict"}`))
				return
			}
			content = append(content, chunk...)
			w.Header().Set("Upload-Offset", strconv.Itoa(len(content)))
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/complete"):
			manifest.CommittedOffset = int64(len(content))
			manifest.State = "published"
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
	defer client.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if _, err := client.SendBatch(ctx, manifest.BatchID, "", []Source{item}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if heads.Load() != 1 || conflicts.Load() != 1 {
		t.Fatalf("heads=%d conflicts=%d, want one refused PATCH and one HEAD", heads.Load(), conflicts.Load())
	}
	if patches.Load() != 2 {
		t.Fatalf("patches=%d, want the refused one plus the resume", patches.Load())
	}
	if !bytes.Equal(content, data) {
		t.Fatal("receiver content differs")
	}
}
