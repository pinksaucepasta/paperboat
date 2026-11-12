//go:build windows

package hostdproto

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"net"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// Generation handover on Windows: the same bytes as handover_unix.go without the descriptors, because a named pipe
// instance or a socket cannot be passed to an unrelated process here (WSADuplicateSocket needs the target's
// process id and a copy; PLAN Log has that later design). The old generation instead gives its fixed names up
// at commit and the new one binds them a few milliseconds later; clients retry an absent pipe or a refused
// connection for up to 2 s (winpipe.Dial, the carrier's worker dial).
//
// Wire, on one connection to HandoverPath(socket), whose pipe DACL admits only SYSTEM, Administrators and the
// enrolled owner:
//
//	new -> old: 32-byte installation token
//	old -> new: 'S'
//	new -> old: 'C' once its worker is ready (started, nothing bound)
//	old -> new: 'C' once it closed its fixed pipes and its worker's loopback listener (accepted work keeps running)
//	new -> old: 'A' once it bound them and activated its worker; the old generation then drains and exits.
//
// A new generation that fails before its 'C' leaves the old one serving. No 'A' within handoverAckTimeout of the
// old one's 'C': the old one binds its names again and keeps serving.

// handoverAckTimeout is a ceiling: the new worker has started every component before its commit and only binds the
// address after it, which takes milliseconds. It was 5 s while the bind waited for the components that call the
// control plane; a slow control plane then made the old generation take the address back before the new one bound
// it ("address already in use", the install timed out: winpool-7, 2026-11-12, staging server).
const handoverAckTimeout = time.Minute

// HandoverConfig is the old generation's side: Listener is the handover pipe it serves now, StopAccepting gives the
// fixed names up and Resume takes them back (binding a new handover pipe that Listener then returns).
type HandoverConfig struct {
	Token         []byte
	Listener      func() net.Listener
	StopAccepting func(context.Context) error
	Resume        func(context.Context) error
}

// ServeHandover answers new generations until one completes the handover, and returns nil then.
func ServeHandover(ctx context.Context, config HandoverConfig) error {
	if len(config.Token) != installationTokenBytes || config.Listener == nil || config.StopAccepting == nil || config.Resume == nil {
		return ErrSocketConfig
	}
	for {
		listener := config.Listener()
		stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
		connection, err := listener.Accept()
		stop()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return causedFailure{message: "hostd handover pipe accept failed", cause: err}
		}
		stopped, err := serveHandoverOne(ctx, connection, config)
		_ = connection.Close()
		if err == nil {
			return nil
		}
		if stopped {
			// The new generation took the commit and did not acknowledge: serve again.
			if resumeErr := config.Resume(ctx); resumeErr != nil {
				return causedFailure{message: "hostd could not serve again after a failed handover", cause: errors.Join(err, resumeErr)}
			}
		}
	}
}

func serveHandoverOne(ctx context.Context, connection net.Conn, config HandoverConfig) (stopped bool, _ error) {
	if err := connection.SetDeadline(time.Now().Add(handoverStepTimeout)); err != nil {
		return false, err
	}
	token := make([]byte, installationTokenBytes)
	if _, err := io.ReadFull(connection, token); err != nil {
		return false, err
	}
	if subtle.ConstantTimeCompare(token, config.Token) != 1 {
		return false, ErrUnauthorized
	}
	if err := writeAll(connection, []byte{handoverStart}); err != nil {
		return false, err
	}
	if err := expectHandoverByte(connection, handoverCommit, handoverCommitTimeout); err != nil {
		return false, err
	}
	if err := config.StopAccepting(ctx); err != nil {
		return true, err
	}
	if err := connection.SetDeadline(time.Now().Add(handoverStepTimeout)); err != nil {
		return true, err
	}
	if err := writeAll(connection, []byte{handoverCommit}); err != nil {
		return true, err
	}
	return true, expectHandoverByte(connection, handoverAck, handoverAckTimeout)
}

// Handover is the new generation's side of one handover connection.
type Handover struct{ connection net.Conn }

// DialHandover authenticates to the generation serving path. ErrNoHandover means no generation serves it.
func DialHandover(ctx context.Context, path string, token []byte) (*Handover, error) {
	if len(token) != installationTokenBytes || !validPipePath(path) {
		return nil, ErrSocketConfig
	}
	dialCtx, cancel := context.WithTimeout(ctx, handoverStepTimeout)
	defer cancel()
	connection, err := winio.DialPipeContext(dialCtx, path)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return nil, ErrNoHandover
	}
	if err != nil {
		return nil, causedFailure{message: "hostd handover pipe connection failed", cause: err}
	}
	h := &Handover{connection: connection}
	if err := h.start(token); err != nil {
		return nil, errors.Join(err, h.Close())
	}
	return h, nil
}

func (h *Handover) start(token []byte) error {
	if err := h.connection.SetDeadline(time.Now().Add(handoverStepTimeout)); err != nil {
		return err
	}
	if err := writeAll(h.connection, token); err != nil {
		return causedFailure{message: "hostd handover token could not be written", cause: err}
	}
	return expectHandoverByte(h.connection, handoverStart, handoverStepTimeout)
}

// Commit asks the old generation to give its names up, runs bind once it has (bind takes the names and activates
// the worker, within handoverAckTimeout), and acknowledges. A bind error is returned without the
// acknowledgement, so the old generation serves again.
func (h *Handover) Commit(bind func() error) error {
	if err := h.connection.SetDeadline(time.Now().Add(handoverStepTimeout)); err != nil {
		return err
	}
	if err := writeAll(h.connection, []byte{handoverCommit}); err != nil {
		return causedFailure{message: "hostd handover commit could not be written", cause: err}
	}
	if err := expectHandoverByte(h.connection, handoverCommit, handoverStepTimeout); err != nil {
		return causedFailure{message: "the running hostd generation did not give its pipes up", cause: err}
	}
	if err := bind(); err != nil {
		return err
	}
	// The activation may have run longer than the commit's step deadline.
	if err := h.connection.SetDeadline(time.Now().Add(handoverStepTimeout)); err != nil {
		return err
	}
	if err := writeAll(h.connection, []byte{handoverAck}); err != nil {
		return causedFailure{message: "hostd handover acknowledgement could not be written", cause: err}
	}
	return nil
}

func (h *Handover) Close() error { return h.connection.Close() }
