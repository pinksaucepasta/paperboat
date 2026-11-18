package inbox

import (
	"os"
	"path/filepath"
	"testing"
)

// The receipt journal 2026.11.16.2 wrote loads (not quarantined): delivered transfers are not delivered again.
func TestLoadsJournalWrittenByPreviousRelease(t *testing.T) {
	raw, err := os.ReadFile("testdata/2026.11.16.2-receipts.json")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".paperboat-receipts.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	value, err := loadJournal(root)
	if err != nil || value.Entries["transfer_01"].Path != "Paperboat Inbox/result.bin" {
		t.Fatalf("previous release's journal = %+v, %v", value, err)
	}
}
