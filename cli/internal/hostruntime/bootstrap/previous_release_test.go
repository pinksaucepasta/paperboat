package bootstrap

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/atomicfile/atomicfiletest"
)

// The resume record 2026.11.16.2 wrote (testdata from that release's SaveResume) loads after an update.
func TestLoadsResumeWrittenByPreviousRelease(t *testing.T) {
	raw, err := os.ReadFile("testdata/2026.11.16.2-bootstrap-resume.json")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	atomicfiletest.WriteOwnerOnly(t, filepath.Join(root, "bootstrap-resume.json"), raw)
	record, err := loadResumeDocument(root)
	if err != nil || record.Alias != "Victus" {
		t.Fatalf("previous release's resume record = %+v, %v", record, err)
	}
}
