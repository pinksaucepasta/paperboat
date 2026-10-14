//go:build darwin || linux

package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/filetransfer"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/operation"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/store"
)

func browserTransferFixture(t *testing.T) (*Dispatcher, Authorization) {
	t.Helper()
	d, root := execDispatcher(t)
	durable, err := store.Open(context.Background(), store.Config{Root: filepath.Join(root, "state")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = durable.Close() })
	service, err := filetransfer.New(filetransfer.Config{Root: filepath.Join(root, "transfers"), PublishRoot: filepath.Join(root, "inbox"), LocalMachineID: "machine_browser", Store: durable})
	if err != nil {
		t.Fatal(err)
	}
	d.config.FileTransfers = service
	created, err := d.config.Sessions.Create(context.Background(), session.CreateRequest{ID: "ses_browser_upload", Name: "upload", Command: pty.Command{Path: "/bin/sh", Args: []string{"-c", "cat"}, CWD: root, Dimensions: pty.Dimensions{Columns: 80, Rows: 24}}})
	if err != nil {
		t.Fatal(err)
	}
	a := Authorization{BrowserTerminal: true, AccountID: "owner", UserID: "owner", MachineID: "machine_browser", SessionID: created.ID, TerminalGeneration: created.Generation, TerminalRole: TerminalRoleOwner, ClientID: "browser_attachment", BrowserAttachmentID: "browser_attachment", ExpiresAt: time.Now().Add(time.Minute)}
	_, err = d.config.Sessions.AttachParticipantAtGeneration(created.ID, session.Participant{AttachmentID: a.BrowserAttachmentID, AccountID: a.AccountID, ClientID: a.ClientID, Role: "owner", Browser: true, ConnectedAt: time.Now()}, 0, created.Generation)
	if err != nil {
		t.Fatal(err)
	}
	return d, a
}
func browserTransferCall(t *testing.T, d *Dispatcher, a Authorization, v any) operation.Outcome {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var decoded browserFileTransferRequest
	if err := decodeStrict(data, &decoded); err != nil {
		t.Fatalf("request decoding: %v", err)
	}
	return d.Handle(context.Background(), a, "file-transfer.v1", data)
}
func browserTransferHash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
func browserTransferCreate(t *testing.T, d *Dispatcher, a Authorization, batch string, data []byte) store.FileTransfer {
	t.Helper()
	out := browserTransferCall(t, d, a, map[string]any{"action": "create", "batch_id": batch, "files": []filetransfer.File{{Basename: "a file ' literal.bin", Size: int64(len(data)), SHA256: browserTransferHash(data)}}})
	if out.ErrorCode != "" {
		t.Fatalf("create %s", out.ErrorCode)
	}
	var result struct {
		Transfers []store.FileTransfer `json:"transfers"`
	}
	if err := json.Unmarshal(out.Result, &result); err != nil || len(result.Transfers) != 1 {
		t.Fatalf("create result err=%v", err)
	}
	return result.Transfers[0]
}
func browserTransferChunk(t *testing.T, d *Dispatcher, a Authorization, id string, offset int64, data []byte, hash string) operation.Outcome {
	return browserTransferCall(t, d, a, map[string]any{"action": "chunk", "transfer_id": id, "offset": offset, "data": base64.StdEncoding.EncodeToString(data), "sha256": hash})
}

func TestBrowserFileTransferResumesAndPublishesVerifiedBatch(t *testing.T) {
	d, a := browserTransferFixture(t)
	data := []byte("browser encrypted file content")
	original := browserTransferCreate(t, d, a, "browser_batch_1", data)
	if out := browserTransferChunk(t, d, a, original.ID, 0, data[:7], browserTransferHash(data[:7])); out.ErrorCode != "" {
		t.Fatal(out.ErrorCode)
	}
	if out := browserTransferCall(t, d, a, map[string]any{"action": "complete", "transfer_id": original.ID}); out.ErrorCode != "invalid_size" {
		t.Fatalf("partial complete %s", out.ErrorCode)
	}
	// A replacement ticket/attachment preserves source ownership, without persisting a browser key.
	fresh := a
	fresh.ClientID = "fresh_attachment"
	fresh.BrowserAttachmentID = fresh.ClientID
	_, err := d.config.Sessions.AttachParticipantAtGeneration(a.SessionID, session.Participant{AttachmentID: fresh.ClientID, AccountID: a.AccountID, ClientID: fresh.ClientID, Role: "owner", Browser: true, ConnectedAt: time.Now()}, 0, a.TerminalGeneration)
	if err != nil {
		t.Fatal(err)
	}
	resumed := browserTransferCreate(t, d, fresh, "browser_batch_1", data)
	if resumed.ID != original.ID || resumed.CommittedOffset != 7 {
		t.Fatalf("resume changed id or offset %#v", resumed)
	}
	if out := browserTransferChunk(t, d, fresh, original.ID, 7, data[7:], browserTransferHash(data[7:])); out.ErrorCode != "" {
		t.Fatal(out.ErrorCode)
	}
	out := browserTransferCall(t, d, fresh, map[string]any{"action": "complete", "transfer_id": original.ID})
	if out.ErrorCode != "" {
		t.Fatal(out.ErrorCode)
	}
	var completed struct {
		Transfer store.FileTransfer `json:"transfer"`
	}
	if err := json.Unmarshal(out.Result, &completed); err != nil {
		t.Fatal(err)
	}
	if completed.Transfer.State != "published" || !filepath.IsAbs(completed.Transfer.ReceiptPath) {
		t.Fatalf("completion %#v", completed.Transfer)
	}
	content, err := os.ReadFile(completed.Transfer.ReceiptPath)
	if err != nil || !bytes.Equal(content, data) {
		t.Fatalf("published integrity err=%v", err)
	}
	out = browserTransferCall(t, d, fresh, map[string]any{"action": "get", "transfer_id": original.ID})
	if out.ErrorCode != "" {
		t.Fatal(out.ErrorCode)
	}
	var inspected struct {
		Transfer store.FileTransfer `json:"transfer"`
	}
	_ = json.Unmarshal(out.Result, &inspected)
	if inspected.Transfer.ReceiptPath != completed.Transfer.ReceiptPath {
		t.Fatal("get did not return actual published path")
	}
}

func TestBrowserFileTransferRejectsAuthorityAndMalformedChunks(t *testing.T) {
	d, a := browserTransferFixture(t)
	data := []byte("verified")
	transfer := browserTransferCreate(t, d, a, "browser_batch_security", data)
	for _, name := range []string{"viewer", "interactive", "native", "account", "machine", "session", "generation", "revoked", "expired", "attachment"} {
		t.Run(name, func(t *testing.T) {
			bad := a
			switch name {
			case "viewer":
				bad.TerminalRole = TerminalRoleViewer
			case "interactive":
				bad.TerminalRole = TerminalRoleInteractive
			case "native":
				bad.BrowserTerminal = false
			case "account":
				bad.AccountID = "other"
				bad.UserID = "other"
			case "machine":
				bad.MachineID = "other"
			case "session":
				bad.SessionID = "other"
			case "generation":
				bad.TerminalGeneration++
			case "revoked":
				bad.Revoked = &atomic.Bool{}
				bad.Revoked.Store(true)
			case "expired":
				bad.ExpiresAt = time.Now().Add(-time.Second)
			case "attachment":
				bad.BrowserAttachmentID = "other"
				bad.ClientID = "other"
			}
			out := browserTransferCall(t, d, bad, map[string]any{"action": "get", "transfer_id": transfer.ID})
			if out.ErrorCode == "" {
				t.Fatal("unauthorized transfer access")
			}
		})
	}
	for _, test := range []struct {
		name string
		data []byte
		hash string
		code string
	}{{"digest mismatch", data, browserTransferHash([]byte("wrong")), "digest_mismatch"}, {"oversized", make([]byte, BrowserFileTransferChunkBytes+1), browserTransferHash(nil), "invalid_request"}, {"uppercase", data, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "invalid_request"}} {
		t.Run(test.name, func(t *testing.T) {
			out := browserTransferChunk(t, d, a, transfer.ID, 0, test.data, test.hash)
			if out.ErrorCode != test.code {
				t.Fatalf("got %s expected %s", out.ErrorCode, test.code)
			}
		})
	}
	if out := browserTransferCall(t, d, a, map[string]any{"action": "get", "transfer_id": transfer.ID, "offset": 0}); out.ErrorCode != "invalid_request" {
		t.Fatal("unused field accepted")
	}
	changed := browserTransferCall(t, d, a, map[string]any{"action": "create", "batch_id": "browser_batch_security", "files": []filetransfer.File{{Basename: "changed.bin", Size: int64(len(data)), SHA256: browserTransferHash(data)}}})
	if changed.ErrorCode != "operation_conflict" {
		t.Fatal("changed resume accepted")
	}
	out := browserTransferCall(t, d, a, map[string]any{"action": "cancel", "transfer_id": transfer.ID})
	if out.ErrorCode != "" {
		t.Fatal(out.ErrorCode)
	}
	if out := browserTransferChunk(t, d, a, transfer.ID, 0, data, browserTransferHash(data)); out.ErrorCode != "canceled" {
		t.Fatalf("canceled write %s", out.ErrorCode)
	}
}

func TestBrowserFileTransferClosedRevocationCancelsBeforeWrite(t *testing.T) {
	d, a := browserTransferFixture(t)
	signal := make(chan struct{})
	close(signal)
	a.RevokedSignal = signal
	out := browserTransferCall(t, d, a, map[string]any{"action": "policy"})
	if out.ErrorCode != "operation_canceled" {
		t.Fatalf("closed revocation accepted: %s", out.ErrorCode)
	}
}

func TestBrowserFileTransferWholeFileIntegrityAndConcurrentResume(t *testing.T) {
	d, a := browserTransferFixture(t)
	data := []byte("verified")
	original := browserTransferCreate(t, d, a, "browser_batch_concurrent", data)
	done := make(chan store.FileTransfer, 2)
	for range 2 {
		go func() {
			out := browserTransferCall(t, d, a, map[string]any{"action": "create", "batch_id": "browser_batch_concurrent", "files": []filetransfer.File{{Basename: original.Basename, Size: original.Size, SHA256: original.SHA256}}})
			var resumed struct {
				Transfers []store.FileTransfer `json:"transfers"`
			}
			if out.ErrorCode != "" || json.Unmarshal(out.Result, &resumed) != nil || len(resumed.Transfers) != 1 {
				done <- store.FileTransfer{}
				return
			}
			done <- resumed.Transfers[0]
		}()
	}
	for range 2 {
		if resumed := <-done; resumed.ID != original.ID {
			t.Fatal("concurrent resume duplicated or changed transfer")
		}
	}
	tampered := []byte("tampered")
	if out := browserTransferChunk(t, d, a, original.ID, 0, tampered, browserTransferHash(tampered)); out.ErrorCode != "" {
		t.Fatal(out.ErrorCode)
	}
	if out := browserTransferCall(t, d, a, map[string]any{"action": "complete", "transfer_id": original.ID}); out.ErrorCode != "digest_mismatch" {
		t.Fatalf("whole digest %s", out.ErrorCode)
	}
	if _, err := d.config.FileTransfers.ExistingPublishedPath(context.Background(), original.ID); err == nil {
		t.Fatal("tampered content published")
	}
}

func TestBrowserFileTransferBatchResumeAndAtomicPublication(t *testing.T) {
	d, a := browserTransferFixture(t)
	data := [][]byte{[]byte("first file"), []byte("second file")}
	files := []filetransfer.File{{Basename: "first.txt", Size: int64(len(data[0])), SHA256: browserTransferHash(data[0])}, {Basename: "second.txt", Size: int64(len(data[1])), SHA256: browserTransferHash(data[1])}}
	create := func() []store.FileTransfer {
		t.Helper()
		out := browserTransferCall(t, d, a, map[string]any{"action": "create", "batch_id": "browser_multifile_batch", "files": files})
		if out.ErrorCode != "" {
			t.Fatal(out.ErrorCode)
		}
		var result struct {
			Transfers []store.FileTransfer `json:"transfers"`
		}
		if err := json.Unmarshal(out.Result, &result); err != nil || len(result.Transfers) != 2 {
			t.Fatalf("batch result err=%v", err)
		}
		return result.Transfers
	}
	original := create()
	if out := browserTransferChunk(t, d, a, original[0].ID, 0, data[0], browserTransferHash(data[0])); out.ErrorCode != "" {
		t.Fatal(out.ErrorCode)
	}
	if out := browserTransferCall(t, d, a, map[string]any{"action": "complete", "transfer_id": original[0].ID}); out.ErrorCode != "invalid_size" {
		t.Fatalf("partial batch %s", out.ErrorCode)
	}
	for _, transfer := range original {
		if _, err := d.config.FileTransfers.ExistingPublishedPath(context.Background(), transfer.ID); err == nil {
			t.Fatal("partial batch published")
		}
	}
	resumed := create()
	for i := range original {
		if resumed[i].ID != original[i].ID || resumed[i].Basename != files[i].Basename {
			t.Fatal("batch resume order or identity changed")
		}
	}
	if out := browserTransferChunk(t, d, a, original[1].ID, 0, data[1], browserTransferHash(data[1])); out.ErrorCode != "" {
		t.Fatal(out.ErrorCode)
	}
	for _, transfer := range original {
		if out := browserTransferCall(t, d, a, map[string]any{"action": "complete", "transfer_id": transfer.ID}); out.ErrorCode != "" {
			t.Fatal(out.ErrorCode)
		}
	}
	for i, transfer := range original {
		path, err := d.config.FileTransfers.ExistingPublishedPath(context.Background(), transfer.ID)
		if err != nil {
			t.Fatal(err)
		}
		content, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(content, data[i]) {
			t.Fatalf("batch publication integrity err=%v", err)
		}
	}
}
