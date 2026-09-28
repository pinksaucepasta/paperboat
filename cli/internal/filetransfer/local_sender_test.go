package filetransfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLocalSenderNativeOmitsKeysAndCancelsKnownBatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	known := make(chan struct{})
	var knownOnce sync.Once
	canceled := make(chan struct{}, 1)
	expires := time.Now().UTC().Truncate(time.Second).Add(time.Minute)
	digest := sha256.Sum256([]byte("data"))
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/v1/local-file-transfers":
			var raw map[string]json.RawMessage
			if json.NewDecoder(request.Body).Decode(&raw) != nil || raw["key_material"] != nil || raw["transfer_generation"] != nil {
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			writeJSONTest(writer, http.StatusCreated, map[string]any{"batch_id": "native_batch", "transfers": []Manifest{{TransferID: "ft_generated", BatchID: "native_batch", SourceMachineID: "machine_host", DestinationMachineID: "machine_cli", InitiatingUserID: "user_1", SessionID: "ses_1", Basename: "data.bin", Size: 4, SHA256: hex.EncodeToString(digest[:]), State: "created", ExpiresAt: expires}}})
		case request.Method == http.MethodGet:
			knownOnce.Do(func() { close(known) })
			writeJSONTest(writer, http.StatusOK, Manifest{TransferID: "ft_generated", BatchID: "native_batch", Basename: "data.bin", Size: 4, State: "uploading", ExpiresAt: expires})
		case request.Method == http.MethodPatch:
			writer.WriteHeader(http.StatusNoContent)
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/complete"):
			writeJSONTest(writer, http.StatusOK, Manifest{TransferID: "ft_generated", State: "pending"})
		case request.Method == http.MethodDelete && request.URL.Path == "/v1/local-file-transfers/ft_generated":
			select {
			case canceled <- struct{}{}:
			default:
			}
			writer.WriteHeader(http.StatusNoContent)
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	sender := &LocalSender{Endpoint: server.URL + "/v1/local-file-transfers", Token: strings.Repeat("t", 43), HTTPClient: server.Client()}
	done := make(chan error, 1)
	go func() {
		_, err := sender.SendNativeBatch(ctx, "native_batch", "machine_host", "machine_cli", "user_1", "ses_1", []Source{{Basename: "data.bin", Size: 4, SHA256: digest, Reader: bytes.NewReader([]byte("data"))}}, expires)
		done <- err
	}()
	<-known
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("send error=%v", err)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("native sender did not cancel known resource")
	}
}

func writeJSONTest(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
