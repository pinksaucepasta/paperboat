package main

import (
	"context"
	"net/http"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	"github.com/pinksaucepasta/paperboat/cli/internal/buildinfo"
)

// updatePolicyDependencies are the edges of the pre-command minimum-version check that tests replace.
type updatePolicyDependencies struct {
	fetch        func(context.Context, string, *http.Client) (api.UpdatePolicy, error)
	version      func() string
	cachePath    func(issuer, configPath string) string
	distribution string // only an official build is held to the server's minimum version
}

func defaultUpdatePolicyDependencies() updatePolicyDependencies {
	return updatePolicyDependencies{
		fetch:        api.FetchUpdatePolicy,
		version:      func() string { return buildinfo.Version },
		cachePath:    cliPolicyCachePath,
		distribution: buildinfo.Distribution,
	}
}

func updatePolicyDependenciesOf(ctx context.Context) updatePolicyDependencies {
	return dependenciesIn(ctx, defaultUpdatePolicyDependencies)
}
