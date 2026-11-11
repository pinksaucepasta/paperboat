package inbox

import (
	"context"
	"fmt"
	"os"
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

// F21-159: a receipt journal this build cannot read (a newer version, or damage) no longer fails every delivery:
// it is kept aside byte for byte and a new journal starts.
func TestUnreadableReceiptJournalIsSetAsideAndDeliveryContinues(t *testing.T) {
	for name, body := range map[string]string{"newer version": `{"version":2,"entries":{},"extra":true}`, "damaged": `{"version":1,"entries":`} {
		t.Run(name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "Paperboat Inbox")
			if err := os.MkdirAll(root, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, journalName), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			content := []byte("after the bad journal")
			receiver, err := New(Config{Client: &fakeClient{data: content}, MachineID: "machine_local", SessionID: "session_1", Path: root})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := receiver.Deliver(context.Background(), manifest("ft_after_bad_journal", "ok.txt", content)); err != nil {
				t.Fatalf("delivery blocked by an unreadable journal: %v", err)
			}
			kept, _ := filepath.Glob(filepath.Join(root, journalName+".rejected-*"))
			if len(kept) != 1 {
				t.Fatalf("the unreadable journal was not kept aside: %v", kept)
			}
			if data, err := os.ReadFile(kept[0]); err != nil || string(data) != body {
				t.Fatalf("the kept journal differs: %q %v", data, err)
			}
			if receipts, err := loadJournal(root); err != nil || len(receipts.Entries) != 1 {
				t.Fatalf("new journal %v %v", receipts.Entries, err)
			}
		})
	}
}
