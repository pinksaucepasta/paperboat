package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

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
// API (for example the web console): the API publishes /v1/client-configuration. A server that cannot be
// reached is left to the pairing itself, which reports the network failure.
func checkServerIsAPI(ctx context.Context, client *http.Client, serverURL string) error {
	if client == nil {
		client = http.DefaultClient
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(serverURL, "/")+"/v1/client-configuration", nil)
	if err != nil {
		return nil
	}
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return nil
	}
	defer response.Body.Close()
	if response.StatusCode >= 500 || response.StatusCode == http.StatusTooManyRequests {
		return nil
	}
	var body struct {
		Version string `json:"version"`
	}
	if response.StatusCode == http.StatusOK && json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&body) == nil && body.Version != "" {
		return nil
	}
	return localArgumentError("the Paperboat server URL does not answer as the Paperboat API (is it the web console?); run pb with --server https://<your Paperboat API host>")
}
