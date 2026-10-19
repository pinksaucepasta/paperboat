package main

import (
	"context"
	"errors"
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
			return client.ListTunnelsV1(ctx, cursor, 200)
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

func resolveTunnelSelector(ctx context.Context, client *api.Client, value string) (string, error) {
	if ctx == nil || client == nil || strings.TrimSpace(value) == "" || value != strings.TrimSpace(value) ||
		!validTunnelCLIName(value, 63) && !validTunnelCLIResourceID(value) {
		return "", api.ErrUnsafeTunnelResponse
	}
	canonicalID := ""
	if strings.HasPrefix(value, "tun_") && validTunnelCLIResourceID(value) {
		tunnel, err := retryTunnelRead(ctx, func() (api.Tunnel, error) {
			return client.GetTunnelV1(ctx, value)
		})
		switch {
		case err == nil:
			if tunnel.ID != value {
				return "", api.ErrUnsafeTunnelResponse
			}
			canonicalID = tunnel.ID
		case api.IsNotFound(err):
		default:
			return "", err
		}
	}
	cursor := ""
	seen := map[string]struct{}{cursor: {}}
	matches := make(map[string]struct{})
	if canonicalID != "" {
		matches[canonicalID] = struct{}{}
	}
	for pageNumber := 0; pageNumber < 10; pageNumber++ {
		page, err := retryTunnelRead(ctx, func() (api.TunnelPage, error) {
			return client.ListTunnelsV1(ctx, cursor, 200)
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
			return "", errors.New("tunnel was not found")
		}
		if _, exists := seen[page.NextCursor]; exists {
			return "", api.ErrUnsafeTunnelResponse
		}
		seen[page.NextCursor] = struct{}{}
		cursor = page.NextCursor
	}
	return "", api.ErrUnsafeTunnelResponse
}
