//go:build !windows

package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Recovery must not turn account-controlled journal bytes into native definitions.
func TestLifecycleRecoveryRejectsUntrustedFilesystem(t *testing.T) {
	for _, attack := range []string{"journal-link", "mutable-journal", "directory-link", "foreign-parent"} {
		t.Run(attack, func(t *testing.T) {
			if attack == "foreign-parent" && os.Geteuid() != 0 {
				t.Skip("requires root to exercise separate-account custody")
			}
			root := t.TempDir()
			if err := os.Chmod(root, 0700); err != nil {
				t.Fatal(err)
			}
			state := filepath.Join(root, "lifecycle")
			image := filepath.Join(root, "image")
			if err := os.WriteFile(image, []byte("trusted prior bytes"), 0600); err != nil {
				t.Fatal(err)
			}
			component := newFakeTransactionalComponent("hostd", nil, nil)
			manager, err := NewLifecycleManager(LifecycleConfig{StateRoot: state, NativeInstallation: fileInstallationState{image}, Components: []TransactionalComponent{component}})
			if err != nil {
				t.Fatal(err)
			}
			func() {
				defer func() {
					if recover() == nil {
						t.Fatal("no interruption")
					}
				}()
				_ = manager.Replace(context.Background(), func(ctx context.Context) error {
					if err := component.Install(ctx); err != nil {
						t.Fatal(err)
					}
					panic("interrupted")
				})
			}()
			before, _ := component.Inspect(context.Background())
			journal := manager.journal
			switch attack {
			case "journal-link":
				target := filepath.Join(root, "external")
				if err = os.Rename(journal, target); err == nil {
					err = os.Symlink(target, journal)
				}
			case "mutable-journal":
				err = os.Chmod(journal, 0666)
			case "directory-link":
				target := filepath.Join(root, "external")
				if err = os.Rename(state, target); err == nil {
					err = os.Symlink(target, state)
				}
			case "foreign-parent":
				err = os.Chown(root, 65534, -1)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = manager.Recover(context.Background()); err == nil {
				t.Fatal("untrusted journal accepted")
			}
			after, _ := component.Inspect(context.Background())
			if !sameComponentStatus(before, after) {
				t.Fatal("native state mutated before custody rejection")
			}
			if _, err = os.Lstat(journal); err != nil {
				t.Fatal("recovery removed untrusted evidence", err)
			}
			if attack == "foreign-parent" {
				if err = os.Chown(root, 0, -1); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
