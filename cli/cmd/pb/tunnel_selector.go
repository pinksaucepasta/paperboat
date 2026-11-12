package main

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

func validTunnelCLIResourceID(value string) bool {
	return len(value) >= 3 && len(value) <= 128 && value == strings.TrimSpace(value) && !strings.ContainsAny(value, "\x00\r\n\t /\\?#%")
}

func findTunnelByExactName(ctx context.Context, client *api.Client, name string) (api.Tunnel, error) {
	if ctx == nil || client == nil || name == "" {
		return api.Tunnel{}, api.ErrUnsafeTunnelResponse
	}
	cursor := ""
	seen := make(map[string]struct{})
	for pageNumber := 0; pageNumber < 10; pageNumber++ {
		page, err := retryTunnelRead(ctx, func() (api.TunnelPage, error) {
			return client.ListTunnelsV1Filtered(ctx, cursor, 200, tunnelSelectorFilter(name))
		})
		if err != nil {
			return api.Tunnel{}, err
		}
		for _, tunnel := range page.Items {
			if tunnel.Name == name {
				return tunnel, nil
			}
		}
		if page.NextCursor == "" {
			break
		}
		if _, exists := seen[page.NextCursor]; exists {
			return api.Tunnel{}, api.ErrUnsafeTunnelResponse
		}
		seen[page.NextCursor] = struct{}{}
		cursor = page.NextCursor
	}
	return api.Tunnel{}, api.ErrUnsafeTunnelResponse
}

// tunnelSelectorFilter asks the server for the tunnels whose id, name, address or host contain the selector, so one
// command reads the few tunnels that can match instead of every tunnel of the account (each list page costs the server
// a permission decision per tunnel: with ~240 tunnels every `pb tunnel delete` made two 4 s lists). A selector is
// matched exactly (id or name) on what comes back, as before.
func tunnelSelectorFilter(selector string) url.Values {
	return url.Values{"q": {selector}}
}

func resolveTunnelSelector(ctx context.Context, client *api.Client, value string) (string, error) {
	if ctx == nil || client == nil || strings.TrimSpace(value) == "" || value != strings.TrimSpace(value) ||
		!validTunnelCLIName(value, 63) && !validTunnelCLIResourceID(value) {
		return "", api.ErrUnsafeTunnelResponse
	}
	cursor := ""
	seen := map[string]struct{}{cursor: {}}
	matches := make(map[string]struct{})
	for pageNumber := 0; pageNumber < 10; pageNumber++ {
		page, err := retryTunnelRead(ctx, func() (api.TunnelPage, error) {
			return client.ListTunnelsV1Filtered(ctx, cursor, 200, tunnelSelectorFilter(value))
		})
		if err != nil {
			return "", err
		}
		for _, tunnel := range page.Items {
			if tunnel.ID == value || tunnel.Name == value {
				matches[tunnel.ID] = struct{}{}
				if len(matches) > 1 {
					return "", &TunnelSelectorAmbiguousError{}
				}
			}
		}
		if page.NextCursor == "" {
			if len(matches) == 1 {
				for id := range matches {
					return id, nil
				}
			}
			// a typed refusal, not a bare error: a bare one reaches the user as "could not finish the command, run pb doctor"
			return "", &api.APIError{Status: http.StatusNotFound, Code: "tunnel_not_found", ServerMessage: "The tunnel was not found (tunnel_not_found). Run `pb tunnel list` to see your tunnels."}
		}
		if _, exists := seen[page.NextCursor]; exists {
			return "", api.ErrUnsafeTunnelResponse
		}
		seen[page.NextCursor] = struct{}{}
		cursor = page.NextCursor
	}
	return "", api.ErrUnsafeTunnelResponse
}
