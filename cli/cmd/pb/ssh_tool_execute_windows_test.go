//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/managedssh"
)

func TestManagedToolPathIgnoresGitFirstOnPath(t *testing.T) {
	native, err := managedssh.ResolveOpenSSH()
	if err != nil {
		t.Skip("no native OpenSSH client")
	}
	git := t.TempDir()
	for _, name := range []string{"ssh.exe", "scp.exe", "sftp.exe"} {
		if err := os.WriteFile(filepath.Join(git, name), []byte("fixture"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", git+";"+os.Getenv("PATH"))
	for _, tool := range []string{"scp", "sftp"} {
		path, err := managedToolPath(tool)
		if err != nil || !strings.EqualFold(path, filepath.Join(filepath.Dir(native), tool+".exe")) {
			t.Fatalf("%s = %q, %v", tool, path, err)
		}
	}
}
