package filetransfer

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"testing"
)

// F6-05: a full disk is its own permanent code, whichever write ran out of room.
func TestFullDiskIsInsufficientStorageNotAStorageHiccup(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.ENOSPC, syscall.EDQUOT} {
		var got *Error
		if err := classifyIO(fmt.Errorf("copy: %w", errno)); !errors.As(err, &got) || got.Code != InsufficientStorage {
			t.Fatalf("%v classified as %#v", errno, err)
		}
		if got := storageFailure(errno); got.Code != InsufficientStorage {
			t.Fatalf("%v write failure classified as %q", errno, got.Code)
		}
	}
	if got := storageFailure(syscall.EIO); got.Code != StorageUnavailable {
		t.Fatalf("an I/O error became %q", got.Code)
	}
	if err := classifyIO(context.Canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation changed: %v", err)
	}
}

func TestCreateRefusesABatchTheDisksCannotHoldBeforeAnythingIsSent(t *testing.T) {
	service, _, _ := newService(t)
	published := t.TempDir()
	service.config.PublishRoot = func() (string, error) { return published, nil }
	free := map[string]uint64{service.config.Root: 1 << 40, published: 4 << 20}
	service.config.FreeSpace = func(path string) (uint64, error) { return free[path], nil }
	data := make([]byte, 5<<20)
	request := requestFor(data)
	request.BatchID = "fb_full"
	_, err := service.Create(context.Background(), request)
	var refusal *Error
	if !errors.As(err, &refusal) || refusal.Code != InsufficientStorage || refusal.NeededBytes != 5<<20 || refusal.FreeBytes != 4<<20 {
		t.Fatalf("Create on a full Inbox disk = %#v", err)
	}
	free[published] = 1 << 40
	request.BatchID = "fb_room"
	if _, err := service.Create(context.Background(), request); err != nil {
		t.Fatalf("Create with room: %v", err)
	}
}

// X-37: a file the Inbox's file system cannot hold (FAT32 stops at 4 GiB) is refused before a byte is sent.
func TestCreateRefusesAFileTheInboxFileSystemCannotHold(t *testing.T) {
	service, _, _ := newService(t)
	published := t.TempDir()
	service.config.PublishRoot = func() (string, error) { return published, nil }
	limit := uint64(4 << 20)
	service.config.MaxFile = func(path string) (uint64, string, error) {
		if path != published {
			t.Fatalf("measured %q, not the Inbox", path)
		}
		return limit, "FAT", nil
	}
	request := requestFor(make([]byte, 5<<20))
	request.BatchID = "fb_fat"
	_, err := service.Create(context.Background(), request)
	var refusal *Error
	if !errors.As(err, &refusal) || refusal.Code != FileTooLarge || refusal.FileBytes != 5<<20 || refusal.FileLimit != limit || refusal.FileSystem != "FAT" {
		t.Fatalf("Create of an oversized file = %#v", err)
	}
	limit = 0 // a file system without a per-file limit
	request.BatchID = "fb_ok"
	if _, err := service.Create(context.Background(), request); err != nil {
		t.Fatalf("Create on an unlimited file system: %v", err)
	}
}
