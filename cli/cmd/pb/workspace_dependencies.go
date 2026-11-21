package main

import (
	"context"
	"io"
	"os"

	"github.com/pinksaucepasta/paperboat/cli/internal/selector"
	"golang.org/x/term"
)

// workspaceDependencies are the edges of choosing a workspace that tests replace.
type workspaceDependencies struct {
	choose func(selector.Options) (selector.Item, error)
	// resolutionNotice receives the one line naming the workspace a machine name resolved in.
	resolutionNotice io.Writer
}

func defaultWorkspaceDependencies() workspaceDependencies {
	return workspaceDependencies{choose: selector.Choose, resolutionNotice: noticeWriter(os.Stderr)}
}

// noticeWriter is out when it is a terminal, else io.Discard: when stderr is piped it carries the
// remote command's stderr (pb exec, pb ssh -- cmd), which must pass through exactly.
func noticeWriter(out *os.File) io.Writer {
	if term.IsTerminal(int(out.Fd())) {
		return out
	}
	return io.Discard
}

func workspaceDependenciesOf(ctx context.Context) workspaceDependencies {
	return dependenciesIn(ctx, defaultWorkspaceDependencies)
}
