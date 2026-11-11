package inbox

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
)

// F21-159: the receipt journal is read, changed and rewritten per delivery. Two daemon generations overlap
// during an update (separate Inbox values here, as separate processes have no shared mutex), and without a
// cross-process lock one rewrite drops the other's receipt, so a replayed transfer is delivered twice.
func TestConcurrentDeliveriesFromSeparateInboxesKeepEveryReceipt(t *testing.T) {
	downloads := t.TempDir()
	root := filepath.Join(downloads, "Paperboat Inbox")
	const writers = 12
	var group sync.WaitGroup
	errs := make(chan error, writers)
	for index := range writers {
		group.Add(1)
		go func() {
			defer group.Done()
			content := []byte(fmt.Sprintf("body %d", index))
			receiver, err := New(Config{Client: &fakeClient{data: content}, MachineID: "machine_local", SessionID: "session_1", Path: root})
			if err == nil {
				_, err = receiver.Deliver(context.Background(), manifest(fmt.Sprintf("ft_lock_%d", index), "same.txt", content))
			}
			errs <- err
		}()
	}
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	receipts, err := loadJournal(root)
	if err != nil || len(receipts.Entries) != writers {
		t.Fatalf("journal holds %d receipts (err %v), want %d: a concurrent rewrite dropped one", len(receipts.Entries), err, writers)
	}
}
