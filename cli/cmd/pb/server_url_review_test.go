package main

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/installsource"
	"github.com/spf13/cobra"
)

// An invalid --server on pb install was dropped and the install went on with the inherited server.
func TestInstallRefusesAnInvalidServerFlag(t *testing.T) {
	previous := buildinfo.DefaultServerURL
	buildinfo.DefaultServerURL = ""
	t.Cleanup(func() { buildinfo.DefaultServerURL = previous })
	installed := false
	command := platformInstallCommand()
	command.SetContext(withInstallDependencies(t.Context(), func(d *installDependencies) {
		d.inheritedServer = func() (string, bool) { return "https://api.example.test", true }
		d.runningSource = func() (string, installsource.Source, error) {
			return "/supplied/pb", installsource.Source{Version: "custom-test", Distribution: installsource.Custom, SHA256: "d"}, nil
		}
		d.installExecutable = func(context.Context, string, installsource.Source, string, string) (string, error) {
			installed = true
			return "/commands/pb", nil
		}
		d.awaitingEnrollment = func(context.Context) bool { return false }
	}))
	command.PersistentFlags().String("server", "", "") // the root command's flag
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"--install-dir", "/commands", "--server", "not-a-url"})
	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), "invalid Paperboat server URL") || installed {
		t.Fatalf("install with an invalid --server: err=%v installed=%t", err, installed)
	}
	if result := classifyCLIJSONError(err); result.StateChanged != false {
		t.Fatalf("state_changed = %v (%s)", result.StateChanged, result.Code)
	}
}

// A fresh pb pair with an invalid, blank or console --server created the machine identity before refusing.
func TestFreshPairRefusesABadServerBeforeIdentity(t *testing.T) {
	previous := buildinfo.DefaultServerURL
	buildinfo.DefaultServerURL = ""
	t.Cleanup(func() { buildinfo.DefaultServerURL = previous })
	console := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<!doctype html><html></html>"))
	}))
	defer console.Close()
	for server, want := range map[string]string{"not-a-url": "invalid Paperboat server URL", "   ": noServerURLMessage, console.URL: "HTTP 200"} {
		stateRoot := filepath.Join(t.TempDir(), "Paperboat", "runtime")
		command := pairCommand()
		command.PersistentFlags().String("server", "", "") // the root command's flag
		command.SetOut(&bytes.Buffer{})
		command.SetErr(&bytes.Buffer{})
		command.SetArgs([]string{"--state-root", stateRoot, "--enrollment-token", "SECRETTOKEN123", "--server", server})
		err := command.Execute()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("pair --server %q: %v", server, err)
		}
		if _, statErr := os.Lstat(filepath.Dir(stateRoot)); !errors.Is(statErr, fs.ErrNotExist) {
			t.Fatalf("pair --server %q created state before refusing: %v", server, statErr)
		}
		if result := classifyCLIJSONError(err); result.StateChanged != false {
			t.Fatalf("pair --server %q: state_changed = %v (%s)", server, result.StateChanged, result.Code)
		}
	}
}

// The server check read an unbounded body from a URL that is not yet trusted.
func TestServerCheckBoundsTheResponse(t *testing.T) {
	huge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"version":"1","cli_verification_url":"https://app.example.test/cli/authorize","machines_url":"https://app.example.test/machines","pad":"` + strings.Repeat("x", serverProbeMaxBody) + `"}}`))
	}))
	defer huge.Close()
	if err := checkServerIsAPI(context.Background(), huge.Client(), huge.URL); err == nil {
		t.Fatal("a response past the size limit was read in full and accepted")
	}
}

// pb update and pb auth login dropped an invalid --server silently and kept the installed server.
func TestUpdateAndAuthLoginRefuseAnInvalidServerFlag(t *testing.T) {
	for name, run := range map[string]func(*cobra.Command, []string) error{"update": actionUpdate, "auth login": actionRun(authBrowserLogin)} {
		command := &cobra.Command{Use: "x", RunE: run, SilenceUsage: true, SilenceErrors: true}
		command.PersistentFlags().String("server", "", "") // the root command's flag
		command.Flags().String("from", "", "")
		command.Flags().Bool("force", false, "")
		command.Flags().String("api-token", "", "")
		command.Flags().Bool("fresh", true, "")
		command.SetContext(withInstallDependencies(t.Context(), func(d *installDependencies) {
			d.runningSource = func() (string, installsource.Source, error) {
				t.Fatalf("%s went on past an invalid --server", name)
				return "", installsource.Source{}, nil
			}
		}))
		command.SetArgs([]string{"--server", "not-a-url"})
		err := command.Execute()
		if err == nil || !strings.Contains(err.Error(), "invalid Paperboat server URL") {
			t.Fatalf("%s --server not-a-url: %v", name, err)
		}
		if result := classifyCLIJSONError(err); result.StateChanged != false {
			t.Fatalf("%s: state_changed = %v (%s)", name, result.StateChanged, result.Code)
		}
	}
	// A blank --server counts as not given: no refusal, and the installed server is kept.
	command := &cobra.Command{Use: "x"}
	command.PersistentFlags().String("server", "", "")
	_ = command.PersistentFlags().Set("server", "   ")
	if err := refuseInvalidGivenServer(command); err != nil || selectedInstallServer(command) != "" {
		t.Fatalf("blank --server: err=%v selected=%q", err, selectedInstallServer(command))
	}
}
