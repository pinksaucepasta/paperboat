//go:build !windows

package hostinstall

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInstallJournalRejectsAccountControlledInode(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires privileged installation custody")
	}
	for _, attack := range []string{"symlink", "hardlink", "foreign-owner", "mutable"} {
		t.Run(attack, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "install-journal.json")
			if err := writeJournal(path, installJournal{Schema: journalSchemaV1, Stage: "prepared"}); err != nil {
				t.Fatal(err)
			}
			if _, err := loadJournal(path); err != nil {
				t.Fatal("trusted baseline rejected", err)
			}
			var err error
			switch attack {
			case "symlink":
				target := filepath.Join(root, "saved")
				if err = os.Rename(path, target); err == nil {
					err = os.Symlink(target, path)
				}
			case "hardlink":
				err = os.Link(path, filepath.Join(root, "alias"))
			case "foreign-owner":
				err = os.Chown(path, 65534, -1)
			case "mutable":
				err = os.Chmod(path, 0666)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = loadJournal(path); err == nil {
				t.Fatal("untrusted installation journal accepted")
			}
			if _, err = os.Lstat(path); err != nil {
				t.Fatal("invalid journal discarded", err)
			}
		})
	}
}
