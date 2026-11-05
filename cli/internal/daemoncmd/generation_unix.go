//go:build darwin || linux

package daemoncmd

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/liveconnections"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/localdaemon"
	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
)

// notifyQuit delivers the SIGQUIT pb install sends a daemon generation it replaced.
func notifyQuit() <-chan os.Signal {
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGQUIT)
	return quit
}

// takeDaemonLock waits for the previous generation to release the daemon lock; the result hands it to Run.
func takeDaemonLock(ctx context.Context, paths localapi.Paths) (func(localdaemon.DaemonConfig) localdaemon.DaemonConfig, error) {
	lock, err := localdaemon.WaitDaemonLock(ctx, paths.LockPath, os.Geteuid())
	if err != nil {
		return nil, coordinatorFailure("The previous Paperboat daemon did not hand over its lock; run pb doctor.", err)
	}
	return func(config localdaemon.DaemonConfig) localdaemon.DaemonConfig {
		config.Lock = lock
		return config
	}, nil
}

// generationPruneInterval is how often the live generation removes units of drained generations.
const generationPruneInterval = 10 * time.Minute

// runLiveGeneration publishes the live generation's state for pb status and pb doctor and removes the
// units of generations that finished draining, until ctx ends.
func runLiveGeneration(ctx context.Context, paths localapi.Paths, generation string) {
	executable, executableErr := cliExecutable()
	pruneDrained := func() {
		err := executableErr
		if err == nil {
			err = localdaemon.PruneExitedGenerations(ctx, executable, generation)
		}
		if err != nil && ctx.Err() == nil {
			diagnosticlog.TryWarn("drained daemon generations could not be pruned", "error", err)
		}
	}
	pruneDrained()
	publish := time.NewTicker(2 * time.Second)
	defer publish.Stop()
	prune := time.NewTicker(generationPruneInterval)
	defer prune.Stop()
	published := -1
	for {
		if streams := liveconnections.Held(); streams != published {
			if localdaemon.WriteGenerationState(paths.StateRoot, localdaemon.GenerationState{Generation: generation, PID: os.Getpid(), State: "live", Streams: streams}) == nil {
				published = streams
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-publish.C:
		case <-prune.C:
			pruneDrained()
		}
	}
}

// publishDrain reports this draining generation's held streams and deadline.
func publishDrain(paths localapi.Paths, generation string) func(int, time.Time) {
	return func(streams int, until time.Time) {
		_ = localdaemon.WriteGenerationState(paths.StateRoot, localdaemon.GenerationState{Generation: generation, PID: os.Getpid(), State: "draining", Streams: streams, DrainUntil: until})
	}
}

func withdrawGenerationState(paths localapi.Paths, generation string) {
	_ = localdaemon.RemoveGenerationState(paths.StateRoot, generation)
}
