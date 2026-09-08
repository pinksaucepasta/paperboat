package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/filetransfer"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
)

func TestNativeTransferCommitsOnlyBoundedVerifiedChunks(t *testing.T) {
	for _, scenario := range []string{"valid", "corrupt", "missing_digest", "oversize", "unauthorized"} {
		t.Run(scenario, func(t *testing.T) {
			base, durable := fileTransferTestHandler(t)
			handler, err := NewNativeFileTransferHandler(base.config)
			if err != nil {
				t.Fatal(err)
			}
			data := bytes.Repeat([]byte("a"), protocol.FileTransferChunkBytes)
			if scenario == "oversize" {
				data = append(data, 'a')
			}
			created, err := handler.config.Service.Create(t.Context(), filetransfer.CreateRequest{BatchID: "native_chunk", SourceMachineID: "machine_client", DestinationMachineID: "machine_host", InitiatingUserID: "user_1", SessionID: "ses_1", Files: []filetransfer.File{{Basename: "test.bin", Size: int64(len(data)), SHA256: transferDigest(data)}}})
			if err != nil {
				t.Fatal(err)
			}
			id := created[0].ID
			request := transferRequest(http.MethodPatch, "http://helper.test/v1/file-transfers/"+id+"/content", data)
			request.Header.Set("Content-Type", "application/offset+octet-stream")
			request.Header.Set(HeaderUploadOffset, "0")
			digest := sha256.Sum256(data)
			request.Header.Set(HeaderUploadDigest, "sha256="+hex.EncodeToString(digest[:]))
			switch scenario {
			case "corrupt":
				request.Header.Set(HeaderUploadDigest, "sha256="+transferDigest([]byte("different")))
			case "missing_digest":
				request.Header.Del(HeaderUploadDigest)
			case "unauthorized":
				request.Header.Set("Authorization", "Bearer wrong")
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			current, err := durable.FileTransfer(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "valid" {
				if response.Code != http.StatusNoContent || current.CommittedOffset != int64(len(data)) {
					t.Fatalf("status=%d offset=%d", response.Code, current.CommittedOffset)
				}
			} else if response.Code < 400 || current.CommittedOffset != 0 {
				t.Fatalf("rejected chunk status=%d offset=%d", response.Code, current.CommittedOffset)
			}
		})
	}
}
