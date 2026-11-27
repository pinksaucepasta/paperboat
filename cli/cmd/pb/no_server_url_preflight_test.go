package main

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/installsource"
)

const noServerURLMessage = "this pb build has no Paperboat server URL"

// A release-style Windows pb with no server URL anywhere failed `pb install` late ("installing runtime services",
// state_changed unknown) after writing the installation. A fresh install with no server is refused before any
// step; a reinstall over an installation that has a server keeps it.
func TestFreshInstallWithNoServerURLFailsBeforeWritingAnything(t *testing.T) {
	previous := buildinfo.DefaultServerURL
	buildinfo.DefaultServerURL = ""
	t.Cleanup(func() { buildinfo.DefaultServerURL = previous })
	source := installsource.Source{Version: "custom-test", Distribution: installsource.Custom, SHA256: "source-digest"}
	for _, inherited := range []string{"", "https://api.example.test"} {
		installed := false
		command := platformInstallCommand()
		command.SetContext(withInstallDependencies(t.Context(), func(d *installDependencies) {
			d.inheritedServer = func() (string, bool) { return inherited, true }
			d.runningSource = func() (string, installsource.Source, error) {
				if inherited == "" {
					t.Fatal("install read its source before refusing a missing server URL")
				}
				return "/supplied/pb", source, nil
			}
			d.installExecutable = func(context.Context, string, installsource.Source, string, string) (string, error) {
				installed = true
				return "/commands/pb", nil
			}
			d.awaitingEnrollment = func(context.Context) bool { return false }
		}))
		command.SetOut(&bytes.Buffer{})
		command.SetErr(&bytes.Buffer{})
		command.SetArgs([]string{"--install-dir", "/commands"})
		err := command.Execute()
		if inherited == "" {
			if err == nil || !strings.Contains(err.Error(), noServerURLMessage) || installed {
				t.Fatalf("fresh install without a server: err=%v installed=%t", err, installed)
			}
			if result := classifyCLIJSONError(err); result.StateChanged != false {
				t.Fatalf("state_changed = %v (%s)", result.StateChanged, result.Code)
			}
			continue
		}
		if err != nil || !installed {
			t.Fatalf("reinstall keeping the installed server: err=%v installed=%t", err, installed)
		}
	}
}

// `pb pair` with no server URL failed correctly but first created the machine identity and state directory.
func TestFreshPairWithNoServerURLCreatesNoState(t *testing.T) {
	previous := buildinfo.DefaultServerURL
	buildinfo.DefaultServerURL = ""
	t.Cleanup(func() { buildinfo.DefaultServerURL = previous })
	stateRoot := filepath.Join(t.TempDir(), "Paperboat", "runtime")
	command := pairCommand()
	command.PersistentFlags().String("server", "", "") // the root command's flag
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"--state-root", stateRoot, "--enrollment-token", "SECRETTOKEN123"})
	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), noServerURLMessage) {
		t.Fatalf("pair without a server: %v", err)
	}
	if _, statErr := os.Lstat(filepath.Dir(stateRoot)); !errors.Is(statErr, fs.ErrNotExist) {
		t.Fatalf("pair created state before refusing: %v", statErr)
	}
	if result := classifyCLIJSONError(err); result.StateChanged != false {
		t.Fatalf("state_changed = %v (%s)", result.StateChanged, result.Code)
	}
}
