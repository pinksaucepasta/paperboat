//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// L35: pb uninstall left the ENV hook (a pb() function for the deleted binary) in the shell startup files.
func TestUninstallRemovesTheEnvHookFromEveryShellAndKeepsTheRest(t *testing.T) {
	home := t.TempDir()
	dotfiles := t.TempDir()
	user := "# mine\r\nexport A=1\n\nalias x='y'" // no final newline, a CRLF line
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	// bash: hook first, as install writes it; zsh: a symlink into a dotfiles checkout; fish: hand-edited hook line.
	write(filepath.Join(home, ".bashrc"), envHookLine("bash", "/usr/local/bin/pb")+"\n"+user)
	write(filepath.Join(dotfiles, "zshenv"), user+"\n"+envHookLine("zsh", "/opt/pb")+"\n")
	if err := os.Symlink(filepath.Join(dotfiles, "zshenv"), filepath.Join(home, ".zshenv")); err != nil {
		t.Fatal(err)
	}
	fish := filepath.Join(home, ".config", "fish", "config.fish")
	write(fish, user+"\nset -gx EDITED 1; "+envHookLine("fish", "/x/pb")+"\n")

	removed, err := removeEnvHookFromRCFiles(home)
	if err != nil || len(removed) != 3 {
		t.Fatalf("removed %v, %v", removed, err)
	}
	for path, want := range map[string]string{filepath.Join(home, ".bashrc"): user, filepath.Join(dotfiles, "zshenv"): user + "\n", fish: user + "\n"} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("%s = %q, want %q", path, got, want)
		}
		if info, _ := os.Stat(path); info.Mode().Perm() != 0o640 {
			t.Fatalf("%s mode changed to %v", path, info.Mode())
		}
	}
	if info, err := os.Lstat(filepath.Join(home, ".zshenv")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symlinked ~/.zshenv is no longer a link")
	}
	if removed, err := removeEnvHookFromRCFiles(t.TempDir()); err != nil || len(removed) != 0 {
		t.Fatalf("missing files: %v, %v", removed, err)
	}
	source, err := os.ReadFile("uninstall_command.go")
	if err != nil || !strings.Contains(string(source), "removeEnvHookFromRCFiles(home)") {
		t.Fatal("pb uninstall does not remove the ENV shell hook")
	}
}

func TestEnvHookRemovalGoesOnPastAFileItCannotRead(t *testing.T) {
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".bashrc"), 0o755); err != nil { // unreadable as a file
		t.Fatal(err)
	}
	zsh := filepath.Join(home, ".zshenv")
	if err := os.WriteFile(zsh, []byte("keep\n"+envHookLine("zsh", "/opt/pb")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	removed, err := removeEnvHookFromRCFiles(home)
	if err == nil || len(removed) != 1 || removed[0] != zsh {
		t.Fatalf("removed %v, err %v", removed, err)
	}
	if got, _ := os.ReadFile(zsh); string(got) != "keep\n" {
		t.Fatalf("zshenv = %q", got)
	}
}
