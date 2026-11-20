package main

import (
	"context"
	"os"
	"runtime"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/localapi"
	"github.com/pinksaucepasta/paperboat/cli/internal/localdaemon"
	"golang.org/x/term"
)

// hostDependencies are the machine-level edges many commands share: where this computer's daemon listens, what it
// says, whether a person is at the keyboard, and the platform. Tests replace them to use a private daemon endpoint.
type hostDependencies struct {
	daemonPaths  func() (localapi.Paths, error)
	readSnapshot func(context.Context) (localapi.Snapshot, error) // the daemon's snapshot, read-only
	warmSnapshot func(context.Context) (localapi.Snapshot, error) // the same with a short deadline, for selection
	openBrowser  func(string) error
	isTerminal   func() bool // standard input is a terminal
	euid         func() int
	goos         string
}

func defaultHostDependencies() hostDependencies {
	return hostDependencies{
		daemonPaths:  localdaemon.CurrentUserPaths,
		readSnapshot: readSnapshotWithLookupTimeout,
		warmSnapshot: readWarmSnapshot,
		openBrowser:  platformOpenBrowser,
		isTerminal:   func() bool { return term.IsTerminal(int(os.Stdin.Fd())) },
		euid:         os.Geteuid,
		goos:         runtime.GOOS,
	}
}

func hostDependenciesOf(ctx context.Context) hostDependencies {
	return dependenciesIn(ctx, defaultHostDependencies)
}

func readSnapshotWithLookupTimeout(ctx context.Context) (localapi.Snapshot, error) {
	return readSnapshotFromDaemon(ctx, daemonSnapshotLookupTimeout)
}

func readWarmSnapshot(ctx context.Context) (localapi.Snapshot, error) {
	return readSnapshotFromDaemon(ctx, 300*time.Millisecond)
}

func readSnapshotFromDaemon(ctx context.Context, timeout time.Duration) (localapi.Snapshot, error) {
	paths, err := hostDependenciesOf(ctx).daemonPaths()
	if err != nil {
		return localapi.Snapshot{}, err
	}
	client, err := localapi.NewClient(paths.SocketPath, timeout)
	if err != nil {
		return localapi.Snapshot{}, err
	}
	return client.Snapshot(ctx)
}
