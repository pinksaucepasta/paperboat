//go:build windows

package connectorrotation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
)

func TestWindowsFileJournalRejectsReparsePoints(t *testing.T) {
	t.Run("directory", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "target")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(root, "link")
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("Windows symlink unavailable: %v", err)
		}
		if _, err := OpenFileJournal(filepath.Join(link, "rotation.json")); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("OpenFileJournal error=%v, want invalid configuration", err)
		}
	})
	t.Run("file", func(t *testing.T) {
		path := secureJournalPath(t)
		journal, err := OpenFileJournal(path)
		if err != nil {
			t.Fatal(err)
		}
		manager, _, _, challenge, _ := testRotationFixture(t)
		manager.config.Journal = journal
		if _, err := manager.AcceptChallenge(context.Background(), challenge); err != nil {
			t.Fatal(err)
		}
		target := path + ".target"
		if err := os.Rename(path, target); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Skipf("Windows symlink unavailable: %v", err)
		}
		if _, err := OpenFileJournal(path); !errors.Is(err, ErrJournalCorrupt) {
			t.Fatalf("OpenFileJournal error=%v, want corruption", err)
		}
	})
}

func TestWindowsFileJournalProtectsPrimaryAndBackup(t *testing.T) {
	path := secureJournalPath(t)
	journal, err := OpenFileJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	manager, _, _, challenge, _ := testRotationFixture(t)
	manager.config.Journal = journal
	proof, err := manager.AcceptChallenge(context.Background(), challenge)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AcceptInstall(context.Background(), installForProof(challenge, proof)); err != nil {
		t.Fatal(err)
	}
	descriptor, err := windowsJournalSecurityDescriptor(false)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{path, path + ".bak"} {
		handle, err := windows.CreateFile(windows.StringToUTF16Ptr(candidate), windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
		if err != nil {
			t.Fatal(err)
		}
		trusted := windowsJournalHandleOwnerTrusted(handle)
		protected := windowssecurity.ProtectedHandleDACLMatches(handle, descriptor.String())
		singleLink := windowsJournalHandleHasSingleLink(handle)
		_ = windows.CloseHandle(handle)
		if !trusted || !protected || !singleLink {
			t.Fatalf("journal security %s: trusted_owner=%t protected_dacl=%t single_link=%t", candidate, trusted, protected, singleLink)
		}
	}
	if _, err := OpenFileJournal(path); err != nil {
		t.Fatalf("reopen protected journal: %v", err)
	}
}

func TestWindowsFileJournalRejectsPermissiveDACL(t *testing.T) {
	for _, target := range []string{"primary", "backup"} {
		t.Run(target, func(t *testing.T) {
			path := secureJournalPath(t)
			journal, err := OpenFileJournal(path)
			if err != nil {
				t.Fatal(err)
			}
			manager, _, _, challenge, _ := testRotationFixture(t)
			manager.config.Journal = journal
			proof, err := manager.AcceptChallenge(context.Background(), challenge)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := manager.AcceptInstall(context.Background(), installForProof(challenge, proof)); err != nil {
				t.Fatal(err)
			}
			candidate := path
			if target == "backup" {
				candidate = path + ".bak"
				if err := writePrivateJournalFile(path, []byte("{")); err != nil {
					t.Fatal(err)
				}
			}
			descriptor, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;WD)")
			if err != nil {
				t.Fatal(err)
			}
			absolute, err := descriptor.ToAbsolute()
			if err != nil {
				t.Fatal(err)
			}
			dacl, _, err := absolute.DACL()
			if err != nil {
				t.Fatal(err)
			}
			if err := windows.SetNamedSecurityInfo(candidate, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := OpenFileJournal(path); !errors.Is(err, ErrJournalCorrupt) {
				t.Fatalf("OpenFileJournal error=%v, want corruption", err)
			}
			_ = os.Remove(path)
		})
	}
}
