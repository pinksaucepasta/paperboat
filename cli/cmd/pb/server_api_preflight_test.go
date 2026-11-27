package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/cli/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntimecmd"
	"github.com/pinksaucepasta/paperboat/cli/internal/shorttemp"
)

func TestServerAPIPreflightRefusesTheConsole(t *testing.T) {
	console := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<!doctype html><html></html>"))
	}))
	defer console.Close()
	if !strings.HasPrefix(console.URL, "https://") {
		t.Fatal(console.URL)
	}
	if err := checkServerIsAPI(context.Background(), console.Client(), console.URL); err == nil || !strings.Contains(err.Error(), "--server") || !strings.Contains(err.Error(), "HTTP 200") {
		t.Fatalf("console URL accepted: %v", err)
	}
	notFound := httptest.NewServer(http.NotFoundHandler())
	defer notFound.Close()
	if err := checkServerIsAPI(context.Background(), notFound.Client(), notFound.URL); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("a server without client configuration accepted: %v", err)
	}
	forbidden := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer forbidden.Close()
	if err := checkServerIsAPI(context.Background(), forbidden.Client(), forbidden.URL); err != nil {
		t.Fatalf("a proxy 403 refused pairing: %v", err)
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/client-configuration" {
			http.NotFound(w, r)
			return
		}
		// The server's shared envelope, as the API client tests write it.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"version":"1","cli_verification_url":"https://app.example.test/cli/authorize","machines_url":"https://app.example.test/machines"}}`))
	}))
	defer api.Close()
	if err := checkServerIsAPI(context.Background(), api.Client(), api.URL); err != nil {
		t.Fatal(err)
	}
	bare := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"version":"1"}`)) // JSON, but not the API's envelope
	}))
	defer bare.Close()
	if err := checkServerIsAPI(context.Background(), bare.Client(), bare.URL); err == nil {
		t.Fatal("a 200 without the client configuration envelope accepted")
	}
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	defer down.Close()
	if err := checkServerIsAPI(context.Background(), down.Client(), down.URL); err != nil {
		t.Fatalf("a temporary server failure refused pairing: %v", err)
	}
}

func TestPairWithoutServerURLFailsBeforeBootstrap(t *testing.T) {
	for _, server := range []string{"", "http://console.example.test"} {
		if _, err := pairPreflightServerURL(server); err == nil || !strings.Contains(err.Error(), "--server") {
			t.Fatalf("server %q: %v", server, err)
		}
	}
}

func TestPairCommandWithoutServerURLStopsBeforeBootstrap(t *testing.T) {
	savedServer, savedNoSystemd := buildinfo.DefaultServerURL, hostruntimecmd.NoSystemd
	buildinfo.DefaultServerURL, hostruntimecmd.NoSystemd = "", func() bool { return false }
	defer func() { buildinfo.DefaultServerURL, hostruntimecmd.NoSystemd = savedServer, savedNoSystemd }()
	stateRoot := filepath.Join(shorttemp.Dir(t), "runtime")
	bootstrapped := false
	command := pairCommand()
	command.SetContext(withAuthDependencies(t.Context(), func(d *authDependencies) {
		d.pairBootstrap = func(context.Context, []string, io.Reader, io.Writer, io.Writer) int {
			bootstrapped = true
			return 0
		}
	}))
	command.PersistentFlags().String("server", "", "") // the root command's flag
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"--enrollment-token", "pbe_test", "--state-root", stateRoot})
	if err := command.Execute(); err == nil || !strings.Contains(err.Error(), "--server") {
		t.Fatalf("pair without a server URL: %v", err)
	}
	if bootstrapped {
		t.Fatal("pair reached bootstrap without a server URL")
	}
	if _, err := os.Lstat(stateRoot); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("pair created its state root before refusing: %v", err)
	}
}

func TestInstallChecksTheServerBeforeInstalling(t *testing.T) {
	console := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<!doctype html><html></html>"))
	}))
	defer console.Close()
	touched := false
	command := platformInstallCommand()
	command.SetContext(withInstallDependencies(t.Context(), func(d *installDependencies) {
		d.runningSource = func() (string, installsource.Source, error) {
			touched = true
			return "", installsource.Source{}, errors.New("unexpected")
		}
		d.installExecutable = func(context.Context, string, installsource.Source, string, string) (string, error) {
			touched = true
			return "", errors.New("unexpected")
		}
	}))
	command.PersistentFlags().String("server", "", "") // the root command's flag
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"--server", console.URL})
	if err := command.Execute(); err == nil || !strings.Contains(err.Error(), "HTTP 200") {
		t.Fatalf("install with the console URL: %v", err)
	}
	if touched {
		t.Fatal("install started before the server URL was checked")
	}
}
