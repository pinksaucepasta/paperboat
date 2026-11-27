//go:build windows

package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/localapi"
	"github.com/pinksaucepasta/paperboat/cli/internal/localtestroot"
	"golang.org/x/sys/windows"
)

// cmd/pb tests on a machine with Paperboat installed read the real daemon unless TestMain isolates them.
func TestCommandTestsNeverResolveTheInstalledDaemon(t *testing.T) {
	root := localtestroot.Root()
	if root == "" {
		t.Fatal("TestMain did not set the test local root")
	}
	paths, err := localapi.CurrentPaths(0) // as localdaemon.CurrentUserPaths: os.Getuid() is -1 on Windows
	if err != nil {
		t.Fatal(err)
	}
	appData, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, windows.KF_FLAG_DEFAULT)
	if err != nil {
		t.Fatal(err)
	}
	if dns := localapi.DNSFile(); !strings.HasPrefix(dns, root) {
		t.Fatalf("dns=%q", dns)
	}
	t.Setenv(localtestroot.EnvName, "") // the installed daemon's pipe is the unsalted one
	installed, err := localapi.WindowsPaths(filepath.Join(appData, "Paperboat", "state"), currentSIDForTest(t))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(paths.StateRoot, root) || paths.SocketPath == installed.SocketPath {
		t.Fatalf("paths=%+v installed pipe=%q", paths, installed.SocketPath)
	}
}

func currentSIDForTest(t *testing.T) string {
	t.Helper()
	user, err := windows.GetCurrentThreadEffectiveToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	return user.User.Sid.String()
}
