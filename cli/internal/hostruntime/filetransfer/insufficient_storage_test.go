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
