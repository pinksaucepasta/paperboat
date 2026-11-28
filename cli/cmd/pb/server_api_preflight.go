package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	"github.com/pinksaucepasta/paperboat/cli/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/cli/internal/config"
)

// pairPreflightServerURL fails a fresh pairing early when the build or flags give no valid server URL.
func pairPreflightServerURL(serverURL string) (string, error) {
	if strings.TrimSpace(serverURL) == "" {
		return "", localArgumentError("this pb build has no Paperboat server URL; run pb with --server https://<your Paperboat API host>")
	}
	normalized, err := config.NormalizeServerURL(serverURL)
	if err != nil {
		return "", localArgumentError("invalid Paperboat server URL; run pb with --server https://<your Paperboat API host>")
	}
	return normalized, nil
}

// checkServerIsAPI refuses, before anything is installed, a server URL that answers but is not the Paperboat
// API (for example the web console). It reads /v1/client-configuration through the shared API client, so the
// response envelope and validation are the ones pairing uses. Refused: a 404, or a 200 that is not a valid
// client configuration. Everything else (no answer, 5xx, 429, a proxy's 401/403/407) is left to the pairing.
func checkServerIsAPI(ctx context.Context, client *http.Client, serverURL string) error {
	if client == nil {
		client = newLoginHTTPClient()
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	recorder := &statusRecorder{next: client.Transport, limit: serverProbeMaxBody}
	if recorder.next == nil {
		//paperboat:allow-source-policy default-http owner=auth reason=nil-transport-means-standard-transport
		recorder.next = http.DefaultTransport
	}
	recorded := *client
	recorded.Transport = recorder
	_, err := api.New(serverURL, config.Credential{}, &recorded).ClientConfiguration(ctx)
	status := recorder.status()
	if err == nil || (status != http.StatusOK && status != http.StatusNotFound) {
		return nil
	}
	return localArgumentError(fmt.Sprintf("the Paperboat server URL does not answer as the Paperboat API (HTTP %d from /v1/client-configuration; is it the web console?); run pb with --server https://<your Paperboat API host>", status))
}

// installPreflightServer checks, before pb install changes anything, the server the installed service will
// use: the --server choice or the build's default. With neither, an existing installation's server is kept.
func installPreflightServer(command *cobra.Command) error {
	// A --server given is checked as given: an invalid or non-API choice is refused, never replaced.
	server := strings.TrimSpace(workspaceServerOverride(command))
	if server == "" {
		server = strings.TrimSpace(buildinfo.DefaultServerURL)
	}
	if server == "" {
		// A fresh install with no server anywhere (no flag, build default, saved configuration or installation)
		// would fail only after writing the installation; refuse it before anything is written.
		if inherited := installDependenciesOf(command.Context()).inheritedServer; inherited != nil {
			if saved, known := inherited(); known && strings.TrimSpace(saved) == "" {
				_, err := pairPreflightServerURL("")
				return err
			}
		}
		return nil
	}
	normalized, err := pairPreflightServerURL(server)
	if err != nil {
		return err
	}
	return checkServerIsAPI(command.Context(), authDependenciesOf(command.Context()).httpClient(), normalized)
}

// serverProbeMaxBody bounds what the server check reads: a client configuration is small, and the URL is
// not yet trusted.
const serverProbeMaxBody = 64 << 10

// statusRecorder keeps the status of the last response a client received (0: none) and, with a limit,
// fails a body read past it.
type statusRecorder struct {
	next  http.RoundTripper
	limit int64
	mu    sync.Mutex
	last  int
}

func (r *statusRecorder) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := r.next.RoundTrip(request)
	if err == nil {
		r.mu.Lock()
		r.last = response.StatusCode
		r.mu.Unlock()
		if r.limit > 0 && response.Body != nil {
			response.Body = &cappedBody{ReadCloser: response.Body, left: r.limit}
		}
	}
	return response, err
}

// cappedBody returns an error once more than its limit is read.
type cappedBody struct {
	io.ReadCloser
	left int64
}

func (b *cappedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		var one [1]byte
		if n, _ := b.ReadCloser.Read(one[:]); n > 0 {
			return 0, errors.New("server response exceeds the size limit")
		}
		return b.ReadCloser.Read(p)
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.ReadCloser.Read(p)
	b.left -= int64(n)
	return n, err
}

func (r *statusRecorder) status() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

// pairServerURLBeforeIdentity checks, for a fresh pairing (no machine registration in stateRoot), the server
// it will use (--server, else the build's) before anything is written under stateRoot: missing, invalid or
// not the Paperboat API is refused.
func pairServerURLBeforeIdentity(command *cobra.Command, stateRoot string) error {
	if _, err := os.Lstat(filepath.Join(stateRoot, "machine-registration.json")); !errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	server, _ := command.Flags().GetString("server")
	server = strings.TrimSpace(server)
	if server == "" {
		server = strings.TrimSpace(buildinfo.DefaultServerURL)
	}
	normalized, err := pairPreflightServerURL(server)
	if err != nil {
		return err
	}
	return checkServerIsAPI(command.Context(), authDependenciesOf(command.Context()).httpClient(), normalized)
}
