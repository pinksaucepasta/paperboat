// Package liveconnections counts the long-lived connections the local daemon carries on behalf of
// commands: SSH-family streams (ssh, scp, sftp, rsync) and local access forwards. An update replaces
// the daemon and ends them, so the updater asks for these counts before it starts.
package liveconnections

import (
	"errors"
	"sync/atomic"

	"github.com/pinksaucepasta/paperboat/shared/diagnosticlog"
)

// Counts is the number of connections open right now.
type Counts struct {
	SSH      int `json:"ssh"`
	Forwards int `json:"forwards"`
}

// Total is every open connection.
func (c Counts) Total() int { return c.SSH + c.Forwards }

var ssh, forwards atomic.Int64

// TrackSSH records one open SSH-family stream; call the returned function once when it closes.
func TrackSSH() func() { return track(&ssh) }

// TrackForward records one open local access forward connection; call the returned function once
// when it closes.
func TrackForward() func() { return track(&forwards) }

func track(counter *atomic.Int64) func() {
	counter.Add(1)
	var done atomic.Bool
	return func() {
		if done.CompareAndSwap(false, true) {
			counter.Add(-1)
		}
	}
}

// Current returns the connections open now.
func Current() Counts { return Counts{SSH: int(ssh.Load()), Forwards: int(forwards.Load())} }

// RecordUnconfirmed leaves a diagnostic record when the daemon could not say how many connections are open. The
// update proceeds anyway (the updater must work when the app is broken), but a session it cuts can then be explained.
func RecordUnconfirmed(source string, err error) {
	if err == nil {
		err = errors.New("no reason given")
	}
	diagnosticlog.TryWarn("open connections could not be confirmed; the update proceeds", "source", source, "error", err.Error())
}

// Combine adds the SSH connections a machine serves (the carrier's count, incoming) to what its daemon carries as a
// client: an update cuts both. It fails only when neither side can answer.
func Combine(daemon Counts, daemonErr error, incoming int, carrierErr error) (Counts, error) {
	if daemonErr != nil && carrierErr != nil {
		return Counts{}, errors.Join(daemonErr, carrierErr)
	}
	if daemonErr != nil {
		daemon = Counts{}
	}
	if carrierErr == nil {
		daemon.SSH += incoming
	}
	return daemon, nil
}
