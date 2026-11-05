package filetransfer

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/store"
)

// An incoming transfer whose sender stopped (killed pb send) must not hold a receive slot for the
// whole retention: with no progress for the delivery timeout it expires and frees the slot, while a
// batch that keeps making progress on any member stays alive (tus expiration, S3 abort-incomplete-multipart).
func TestAbandonedIncomingTransferFreesItsSlotAfterIdleTimeout(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	root := t.TempDir()
	durable, err := store.Open(context.Background(), store.Config{Root: filepath.Join(root, "state")})
	if err != nil {
		t.Fatal(err)
	}
	defer durable.Close()
	service, err := New(Config{Root: filepath.Join(root, "transfers"), LocalMachineID: "machine_local", Store: durable, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	idle := time.Duration(DefaultPolicy.DeliveryTimeoutSeconds) * time.Second
	data := []byte("abcdefgh")
	abandoned := requestFor(data)
	abandoned.BatchID = "abandoned"
	if _, err := service.Create(context.Background(), abandoned); err != nil {
		t.Fatal(err)
	}
	active := requestFor(data)
	active.BatchID = "active"
	active.Files = append(active.Files, File{Basename: "second.bin", Size: int64(len(data)), SHA256: digest(data)})
	members, err := service.Create(context.Background(), active)
	if err != nil {
		t.Fatal(err)
	}
	// Both slots are taken; the active batch makes progress on its first file just before the deadline.
	now = now.Add(idle - time.Second)
	if _, err := service.Append(context.Background(), members[0].ID, 0, bytes.NewReader(data[:4])); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	if err := service.CleanupExpired(context.Background()); err != nil {
		t.Fatal(err)
	}
	next := requestFor(data)
	next.BatchID = "next"
	if _, err := service.Create(context.Background(), next); err != nil {
		t.Fatalf("an abandoned transfer still holds its receive slot after %s idle: %v", idle, err)
	}
	for _, member := range members {
		if manifest, err := service.Get(context.Background(), member.ID); err != nil || (manifest.State != "created" && manifest.State != "uploading") {
			t.Fatalf("a batch with recent progress expired: %#v err=%v", manifest, err)
		}
	}
}
