package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"
)

// guardPACRefresh is how often hostd re-sends its PAC (plus up to a sixth of jitter): a restarted guard has lost
// every registration, so previews are back within guardPACRefresh * 7/6.
const guardPACRefresh = 60 * time.Second

// ErrMachineGuardUnavailable is why private previews are degraded when the root machine guard cannot be reached.
// It names the repair; the host reports it as the preview_recovery component's reason and pb doctor shows it.
var ErrMachineGuardUnavailable = errors.New("private previews need the machine guard, which is not reachable; install it with: sudo pb daemon machine-guard install")

// guardPAC hands this user's private-preview PAC to the root machine guard (macOS), which serves it from the one
// system auto-proxy URL. hostd never changes a system proxy setting itself.
type guardPAC struct {
	// set registers body with the guard (empty withdraws); after an error the next call reconnects.
	set      func(ctx context.Context, body string) error
	fetch    func(ctx context.Context, url string) (string, error)
	interval time.Duration // zero means guardPACRefresh
	state    *guardPACState
}

type guardPACState struct {
	mu     sync.Mutex
	body   string
	cancel context.CancelFunc
	done   chan struct{}
}

func newGuardPAC(set func(context.Context, string) error, interval time.Duration) guardPAC {
	return guardPAC{set: set, fetch: fetchLoopbackPAC, interval: interval, state: &guardPACState{}}
}

// send registers body, reconnecting once: a guard restart closes the connection, and the first send on it fails.
func (g guardPAC) send(ctx context.Context, body string) error {
	err := g.set(ctx, body)
	if err != nil && !errors.Is(err, ErrMachineGuardUnavailable) {
		err = g.set(ctx, body)
	}
	return err
}

func (g guardPAC) Recover(context.Context) error { return nil } // the guard drops the PAC when hostd's connection closes

func (g guardPAC) Install(ctx context.Context, pacURL string) error {
	body, err := g.fetch(ctx, pacURL)
	if err != nil {
		return err
	}
	if err := g.send(ctx, body); err != nil {
		return err
	}
	g.state.mu.Lock()
	defer g.state.mu.Unlock()
	g.state.body = body
	if g.state.cancel == nil {
		loopCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		g.state.cancel, g.state.done = cancel, make(chan struct{})
		go g.refreshLoop(loopCtx, g.state.done)
	}
	return nil
}

// refreshLoop re-sends the registered PAC so a restarted guard serves it again within the refresh bound.
func (g guardPAC) refreshLoop(ctx context.Context, done chan struct{}) {
	defer close(done)
	interval := g.interval
	if interval == 0 {
		interval = guardPACRefresh
	}
	for {
		timer := time.NewTimer(interval + rand.N(interval/6+1))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		g.state.mu.Lock()
		body := g.state.body
		g.state.mu.Unlock()
		attempt, cancel := context.WithTimeout(ctx, 10*time.Second)
		_ = g.send(attempt, body) // a down guard is retried at the next tick; the start failure already reported it
		cancel()
	}
}

func (g guardPAC) Refresh(ctx context.Context, pacURL string) error { return g.Install(ctx, pacURL) }

func (g guardPAC) Remove(ctx context.Context) error {
	g.state.mu.Lock()
	cancel, done := g.state.cancel, g.state.done
	g.state.cancel = nil
	g.state.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
	return g.send(ctx, "")
}

// fetchLoopbackPAC reads the PAC hostd's own proxy serves; the URL is hostd's loopback listener.
func fetchLoopbackPAC(ctx context.Context, url string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("read the private preview PAC: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("read the private preview PAC: status %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 32<<10+1))
	if err != nil || len(body) > 32<<10 {
		return "", fmt.Errorf("read the private preview PAC: %w (limit 32 KiB)", err)
	}
	return string(body), nil
}
