package filetransfer

import (
	"bytes"
	"context"
	"crypto/sha256"
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

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
)

func TestNativeClientExplicitCancellationRemovesKnownTransfer(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	item := source([]byte("canceled payload"))
	manifest := Manifest{TransferID: "ft_cancel", BatchID: "batch_cancel", SourceMachineID: "machine_source", DestinationMachineID: "machine_destination", InitiatingUserID: "user_1", SessionID: "session_cancel", Basename: item.Basename, Size: item.Size, SHA256: hex.EncodeToString(item.SHA256[:])}
	var canceled atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			_ = json.NewEncoder(w).Encode(Batch{BatchID: manifest.BatchID, Transfers: []Manifest{manifest}})
		case http.MethodHead:
			cancel()
			w.Header().Set("Upload-Offset", "0")
		case http.MethodDelete:
			canceled.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request: %s", r.Method)
			w.WriteHeader(400)
		}
	}))
	defer server.Close()
	client, err := NewNativeClient(server.URL+"/v1/file-transfers", Auth{Token: "token"}, testBinding(), func(ctx context.Context) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_, err = client.SendBatch(ctx, manifest.BatchID, manifest.SessionID, []Source{item})
	if !errors.Is(err, context.Canceled) || canceled.Load() != 1 {
		t.Fatalf("cancel err=%v deletes=%d", err, canceled.Load())
	}
}

func TestNativeClientResumesLostCommitAndRetainsBatchAcrossOutage(t *testing.T) {
	for _, outage := range []bool{false, true} {
		t.Run(strconv.FormatBool(outage), func(t *testing.T) {
			data := bytes.Repeat([]byte("native"), (2*protocol.FileTransferChunkBytes)/6+3)
			item := source(data)
			manifest := Manifest{TransferID: "ft_native", BatchID: "batch_native", SourceMachineID: "machine_source", DestinationMachineID: "machine_destination", InitiatingUserID: "user_1", SessionID: "session_native", Basename: item.Basename, Size: item.Size, SHA256: hex.EncodeToString(item.SHA256[:]), State: "uploading"}
			var mu sync.Mutex
			var content []byte
			var offsets []int64
			lost, deletes := false, 0
			unavailable := outage
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.Header.Get("Authorization") != "Bearer native_token" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/v1/file-transfers":
					_ = json.NewEncoder(w).Encode(Batch{BatchID: manifest.BatchID, Transfers: []Manifest{manifest}})
				case r.Method == http.MethodHead:
					w.Header().Set("Upload-Offset", strconv.Itoa(len(content)))
				case r.Method == http.MethodPatch:
					offset, _ := strconv.ParseInt(r.Header.Get("Upload-Offset"), 10, 64)
					offsets = append(offsets, offset)
					chunk, err := io.ReadAll(r.Body)
					digest := sha256.Sum256(chunk)
					if err != nil || len(chunk) > protocol.FileTransferChunkBytes || r.Header.Get("Upload-Digest") != "sha256="+hex.EncodeToString(digest[:]) || offset != int64(len(content)) {
						t.Error("invalid bounded commit")
						w.WriteHeader(400)
						return
					}
					if !unavailable {
						content = append(content, chunk...)
					}
					if !lost || unavailable {
						lost = true
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						_ = conn.Close()
						return
					}
					w.Header().Set("Upload-Offset", strconv.Itoa(len(content)))
					w.WriteHeader(http.StatusNoContent)
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/complete"):
					manifest.CommittedOffset = int64(len(content))
					manifest.State = "published"
					_ = json.NewEncoder(w).Encode(map[string]any{"transfer": manifest, "result": map[string]string{"code": "published", "path": "Paperboat Inbox/data.bin"}})
				case r.Method == http.MethodDelete:
					deletes++
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected method %s", r.Method)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			newClient := func() *NativeClient {
				c, err := NewNativeClient(server.URL+"/v1/file-transfers", Auth{Token: "native_token"}, testBinding(), func(ctx context.Context) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "tcp", server.Listener.Addr().String())
				})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = c.Close() })
				return c
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			client := newClient()
			batch, err := client.SendBatch(ctx, manifest.BatchID, manifest.SessionID, []Source{item})
			if outage {
				var resume *ResumeRequiredError
				if !errors.As(err, &resume) || resume.BatchID != manifest.BatchID || batch.BatchID != manifest.BatchID {
					t.Fatalf("outage: %v batch=%s", err, batch.BatchID)
				}
				mu.Lock()
				if deletes != 0 {
					t.Error("outage canceled resumable state")
				}
				unavailable = false
				mu.Unlock()
				_ = client.Close()
				client = newClient()
				batch, err = client.SendBatch(ctx, manifest.BatchID, manifest.SessionID, []Source{source(data)})
			}
			if err != nil || batch.BatchID != manifest.BatchID {
				t.Fatalf("send: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if !bytes.Equal(content, data) || deletes != 0 {
				t.Fatalf("content mismatch or cancel count=%d", deletes)
			}
			if !outage && (len(offsets) != 3 || offsets[1] != protocol.FileTransferChunkBytes) {
				t.Fatalf("lost commit resent bytes: %v", offsets)
			}
		})
	}
}
