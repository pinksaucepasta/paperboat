package main

import (
	"context"
	"io"
	"os"

	"github.com/pinksaucepasta/paperboat/cli/internal/selector"
)

// workspaceDependencies are the edges of choosing a workspace that tests replace.
type workspaceDependencies struct {
	choose func(selector.Options) (selector.Item, error)
	// resolutionNotice receives the one line naming the workspace a machine name resolved in.
	resolutionNotice io.Writer
}

func defaultWorkspaceDependencies() workspaceDependencies {
	return workspaceDependencies{choose: selector.Choose, resolutionNotice: os.Stderr}
}

func workspaceDependenciesOf(ctx context.Context) workspaceDependencies {
	return dependenciesIn(ctx, defaultWorkspaceDependencies)
}
