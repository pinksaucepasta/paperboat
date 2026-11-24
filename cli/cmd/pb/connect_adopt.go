package main

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	"github.com/pinksaucepasta/paperboat/cli/internal/resolver"
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

// sessionResolveError is reconnectResolveError for a connect's own session. Paperboat not knowing a session opened
// without it only means it is not registered yet: it is registered now and the redial goes on. A session closed
// elsewhere keeps its id (conflict) and a refused machine refuses the registration too; both end the terminal.
func sessionResolveError(ctx context.Context, client offlineSessionAdopter, machineID, sessionID string, err error) error {
	if !resolver.IsOfflineTerminalSessionID(sessionID) || apiErrorCode(err) != "terminal_session_not_found" {
		return reconnectResolveError(err)
	}
	_, adoptErr := client.AdoptOfflineTerminalSession(ctx, machineID, sessionID)
	switch apiErrorCode(adoptErr) {
	case "terminal_session_name_conflict":
		return reconnectResolveError(err)
	case "terminal_session_not_found": // a restarting host may not know its sessions yet
		return err
	}
	if adoptErr != nil {
		return reconnectResolveError(adoptErr)
	}
	return err
}

func apiErrorCode(err error) string {
	var apiErr *api.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Code
	}
	return ""
}
