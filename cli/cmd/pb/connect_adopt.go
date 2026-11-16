package main

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
)

type offlineSessionAdopter interface {
	AdoptOfflineTerminalSession(ctx context.Context, machineID, sessionID string) (api.TerminalSession, error)
}

const (
	adoptFirstRetry = 5 * time.Second
	adoptMaxRetry   = time.Minute
	adoptLastTry    = 5 * time.Second
)

// adoptOfflineSession gets a session opened while Paperboat was unreachable registered with Paperboat once the machine
// is online again, so `pb session list` shows it and it is closed like any other. It retries with backoff while this
// connect lives; finish stops the retries and makes one last attempt. A session whose connect ends before Paperboat
// answers is registered by the next `pb connect --session <id>`.
func adoptOfflineSession(ctx context.Context, client offlineSessionAdopter, machineID, sessionID string, first time.Duration) (finish func()) {
	var done sync.WaitGroup
	var adopted bool
	var mu sync.Mutex
	try := func(ctx context.Context) bool {
		_, err := client.AdoptOfflineTerminalSession(ctx, machineID, sessionID)
		mu.Lock()
		defer mu.Unlock()
		adopted = adopted || err == nil
		return err == nil || errors.Is(err, api.ErrUnauthenticated)
	}
	loopCtx, stop := context.WithCancel(ctx)
	done.Add(1)
	go func() {
		defer done.Done()
		for wait := first; ; wait = min(2*wait, adoptMaxRetry) {
			select {
			case <-loopCtx.Done():
				return
			case <-time.After(wait):
			}
			if try(loopCtx) {
				return
			}
		}
	}()
	return func() {
		stop()
		done.Wait()
		mu.Lock()
		already := adopted
		mu.Unlock()
		if already {
			return
		}
		last, cancel := context.WithTimeout(context.WithoutCancel(ctx), adoptLastTry)
		defer cancel()
		try(last)
	}
}
