package filetransfer

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// X-37: an Inbox copy that fails (a full disk) delivered nothing, so the record says failed with the reason, not
// published. The spool stays for a repeated complete, and goes with the retention deadline when nobody retries.
func TestFailedInboxCopyIsRecordedAsFailedAndCanBePublishedAgain(t *testing.T) {
	service, durable, spool := newService(t)
	inbox := t.TempDir()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	service.config.Now = func() time.Time { return now }
	service.config.PublishRoot = func() (string, error) { return inbox, nil }
	full := true
	service.config.EnsurePublishRoot = func(string) error {
		if full {
			return syscall.ENOSPC
		}
		return nil
	}
	data := []byte("received bytes")
	created, err := service.Create(context.Background(), requestFor(data))
	if err != nil {
		t.Fatal(err)
	}
	id := created[0].ID
	if _, err := service.Append(context.Background(), id, 0, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Complete(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	var failure *Error
	if _, err := service.PublishedPath(context.Background(), id); !errors.As(err, &failure) || !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("PublishedPath on a full disk = %v", err)
	}
	record, err := durable.FileTransfer(context.Background(), id)
	if err != nil || record.State != "failed" || record.ResultCode != string(failure.Code) {
		t.Fatalf("record after the failed copy = %+v (err=%v), want failed with code %q", record, err, failure.Code)
	}
	if _, err := os.Stat(filepath.Join(spool, id+".content")); err != nil {
		t.Fatalf("the spool was dropped: %v", err)
	}

	full = false
	if _, err := service.Complete(context.Background(), id); err != nil {
		t.Fatalf("repeated complete: %v", err)
	}
	published, err := service.PublishedPath(context.Background(), id)
	if err != nil || published != filepath.Join(inbox, "data.bin") {
		t.Fatalf("published=%q err=%v", published, err)
	}
	if record, err := durable.FileTransfer(context.Background(), id); err != nil || record.State != "published" {
		t.Fatalf("record after the retry = %+v (err=%v)", record, err)
	}
}

func TestFailedInboxCopyReleasesItsSpoolAtRetention(t *testing.T) {
	service, durable, spool := newService(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	service.config.Now = func() time.Time { return now }
	service.config.PublishRoot = func() (string, error) { return t.TempDir(), nil }
	service.config.EnsurePublishRoot = func(string) error { return syscall.ENOSPC }
	data := []byte("received bytes")
	created, err := service.Create(context.Background(), requestFor(data))
	if err != nil {
		t.Fatal(err)
	}
	id := created[0].ID
	if _, err := service.Append(context.Background(), id, 0, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Complete(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := service.PublishedPath(context.Background(), id); err == nil {
		t.Fatal("PublishedPath succeeded on a full disk")
	}
	now = now.Add(365 * 24 * time.Hour)
	if err := service.CleanupExpired(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(spool, id+".content")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the spool of a failed copy survived retention: %v", err)
	}
	if record, err := durable.FileTransfer(context.Background(), id); err != nil || record.State != "expired" || record.ResultCode == "" {
		t.Fatalf("record after retention = %+v (err=%v), want expired with the reason kept", record, err)
	}
}
