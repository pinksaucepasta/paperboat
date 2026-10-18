//go:build darwin || linux || windows

package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeInboxFile(t *testing.T, root, name string, size int, modified time.Time) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
}

func TestScanInboxListsDeliveredFilesNewestFirstWithoutHiddenOrNonFiles(t *testing.T) {
	root := t.TempDir()
	base := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	writeInboxFile(t, root, "old.txt", 5, base)
	writeInboxFile(t, root, "new.txt", 12, base.Add(time.Hour))
	writeInboxFile(t, root, ".paperboat-receipts.json", 3, base.Add(2*time.Hour))
	writeInboxFile(t, root, ".paperboat-transfer-abc.part", 9, base.Add(2*time.Hour))
	if err := os.Mkdir(filepath.Join(root, "folder"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "old.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	got, err := scanInbox(root, base.Add(3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got.Total != 2 || len(got.Items) != 2 || got.Items[0].Name != "new.txt" || got.Items[0].Size != 12 || got.Items[1].Name != "old.txt" || !got.Items[0].ReceivedAt.Equal(base.Add(time.Hour)) {
		t.Fatalf("listing = %+v", got)
	}
	if got.Items[0].ID != inboxFileID("new.txt") || got.Items[0].ID == got.Items[1].ID || len(got.Items[0].ID) != 16 {
		t.Fatalf("ids = %q %q", got.Items[0].ID, got.Items[1].ID)
	}
}

func TestScanInboxBoundsTheListAndKeepsTheTotal(t *testing.T) {
	root := t.TempDir()
	base := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	for i := 0; i < inboxObservationLimit+7; i++ {
		writeInboxFile(t, root, fmt.Sprintf("f%03d.bin", i), 1, base.Add(time.Duration(i)*time.Second))
	}
	got, err := scanInbox(root, base)
	if err != nil {
		t.Fatal(err)
	}
	if got.Total != inboxObservationLimit+7 || len(got.Items) != inboxObservationLimit || got.Items[0].Name != fmt.Sprintf("f%03d.bin", inboxObservationLimit+6) {
		t.Fatalf("total=%d items=%d first=%s", got.Total, len(got.Items), got.Items[0].Name)
	}
}

func TestInboxObservationIsSentOnChangeAndRepeatedAfterTheResendInterval(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	writeInboxFile(t, root, "a.txt", 1, now)
	sender := &runtimeObservationSender{inboxPath: root}
	first, print1 := sender.inboxObservationFor(now)
	if first == nil || print1 == "" {
		t.Fatal("first heartbeat must carry the listing")
	}
	// Not accepted yet: the same listing is offered again.
	if again, _ := sender.inboxObservationFor(now.Add(10 * time.Second)); again == nil {
		t.Fatal("an unaccepted listing must be offered again")
	}
	sender.inboxObservationSent(print1, now)
	if same, _ := sender.inboxObservationFor(now.Add(10 * time.Second)); same != nil {
		t.Fatal("an unchanged accepted listing must not be resent")
	}
	writeInboxFile(t, root, "b.txt", 2, now.Add(time.Second))
	if changed, _ := sender.inboxObservationFor(now.Add(20 * time.Second)); changed == nil || changed.Total != 2 {
		t.Fatalf("a changed listing must be sent: %+v", changed)
	}
	if repeated, _ := sender.inboxObservationFor(now.Add(inboxResendAfter + time.Second)); repeated == nil {
		t.Fatal("an unchanged listing is repeated after the resend interval")
	}
	if none, _ := (&runtimeObservationSender{}).inboxObservationFor(now); none != nil {
		t.Fatal("no inbox path, no listing")
	}
}
