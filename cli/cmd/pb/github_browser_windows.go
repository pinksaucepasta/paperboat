//go:build windows

package main

import "context"

func openGitHubAuthorizationBrowser(ctx context.Context, target string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return platformOpenBrowser(target)
}
