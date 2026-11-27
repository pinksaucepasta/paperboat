package main

import (
	"context"
	"fmt"
	"net/http"
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
		client = http.DefaultClient
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	recorder := &statusRecorder{next: client.Transport}
	if recorder.next == nil {
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
	server := selectedInstallServer(command)
	if server == "" {
		server = strings.TrimSpace(buildinfo.DefaultServerURL)
	}
	if server == "" {
		return nil
	}
	normalized, err := pairPreflightServerURL(server)
	if err != nil {
		return err
	}
	return checkServerIsAPI(command.Context(), authDependenciesOf(command.Context()).httpClient(), normalized)
}

// statusRecorder keeps the status of the last response a client received (0: none).
type statusRecorder struct {
	next http.RoundTripper
	mu   sync.Mutex
	last int
}

func (r *statusRecorder) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := r.next.RoundTrip(request)
	if err == nil {
		r.mu.Lock()
		r.last = response.StatusCode
		r.mu.Unlock()
	}
	return response, err
}

func (r *statusRecorder) status() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}
