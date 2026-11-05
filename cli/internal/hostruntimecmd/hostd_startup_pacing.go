package hostruntimecmd

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/shared/backoff"
)

// launchd respawns a KeepAlive job that exits after at most its ThrottleInterval (10 s by default; it "will
// not be spawned more than once every ThrottleInterval seconds", launchd.plist(5)), forever and without
// growth, unlike systemd's RestartSteps. So on macOS a failing hostd paces its
// own restarts: before exiting with an error it waits out a backoff persisted across launches (capped,
// jittered: shared/backoff), and launchd's throttle comes on top. Prior art: tailscale util/backoff.
const (
	hostdStartupRetryFirst   = 5 * time.Second
	hostdStartupRetryMaximum = 5 * time.Minute
	// hostdStableRun is how long hostd must have run for its exit to start a new failure streak, matching the
	// one-minute stable run after which Linux resets systemd's restart backoff.
	hostdStableRun = time.Minute
)

type hostdStartupPacingState struct {
	Failures int `json:"failures"`
}

// hostdStartupPacingPath keeps the failure count next to hostd's fence, in the root-owned hostd directory.
func hostdStartupPacingPath(environ func(string) string) string {
	token := environ("PAPERBOAT_HOSTD_TOKEN_FILE")
	if !filepath.IsAbs(token) {
		return ""
	}
	return filepath.Join(filepath.Dir(token), "hostd", "startup-backoff.json")
}

// paceHostdExit records how hostd ended and, after a failure, waits the backoff for the failure streak before
// the process exits. A clean exit clears the streak; a run of hostdStableRun or more starts a new one. The
// wait ends at once when ctx ends (launchd stopping the job): shutdown always wins. It returns the wait.
func paceHostdExit(ctx context.Context, path string, ranFor time.Duration, failed bool, sleep func(context.Context, time.Duration)) time.Duration {
	if path == "" {
		return 0
	}
	if !failed {
		_ = os.Remove(path)
		return 0
	}
	state := hostdStartupPacingState{}
	if ranFor < hostdStableRun {
		state = readHostdStartupPacing(path)
	}
	state.Failures = min(state.Failures+1, 64)
	if data, err := json.Marshal(state); err == nil {
		_ = atomicfile.Write(path, data, atomicfile.CurrentOwnerOptions(0o600))
	}
	wait := backoff.Retry(state.Failures, hostdStartupRetryFirst, hostdStartupRetryMaximum)
	sleep(ctx, wait)
	return wait
}

func readHostdStartupPacing(path string) hostdStartupPacingState {
	file, err := os.Open(path)
	if err != nil {
		return hostdStartupPacingState{}
	}
	defer file.Close()
	var state hostdStartupPacingState
	if err := json.NewDecoder(io.LimitReader(file, 4<<10)).Decode(&state); err != nil || state.Failures < 0 {
		return hostdStartupPacingState{}
	}
	return state
}

func sleepContext(ctx context.Context, wait time.Duration) {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
