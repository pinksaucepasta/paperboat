//go:build darwin || linux || windows

package machineguard

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/splitdns"
)

// DiscoverBrowserDomain asks the deployment's server which domain local
// access uses; the guard has no built-in one.
func DiscoverBrowserDomain(ctx context.Context, controlURL string) (string, error) {
	serverURL, err := config.NormalizeServerURL(controlURL)
	if err != nil {
		return "", err
	}
	discovery, err := api.New(serverURL, config.Credential{}, &http.Client{Timeout: 15 * time.Second}).ClientConfiguration(ctx)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(discovery.LocalAccessBaseDomain) == "" {
		return "", errors.New("the server did not publish a local access domain; contact the server operator")
	}
	return splitdns.NormalizeBrowserDomain(discovery.LocalAccessBaseDomain)
}
