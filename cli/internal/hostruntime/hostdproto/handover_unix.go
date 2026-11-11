//go:build darwin || linux

package hostdproto

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"net"
	"os"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Generation handover: a new hostd imports the old generation's listening
// sockets instead of binding fresh ones, so the fixed paths and the worker's
// loopback port keep accepting while an update replaces the processes.
// Prior art: Envoy hot restart (listener sockets passed with SCM_RIGHTS between
// unrelated processes over a Unix socket), Teleport lib/service/signals.go
// (forkChild + importFileDescriptors, child readiness before the parent's
// graceful Shutdown) and nginx binary upgrade (~/ref/nginx src/core/nginx.c
// ngx_add_inherited_sockets; graceful QUIT in src/core/ngx_config.h).
//
// Wire, on one connection to HandoverPath(socket), both sides checking the
// peer is root or the enrolled UID:
//
//	new -> old: 32-byte installation token
//	old -> new: 'S' + SCM_RIGHTS(start files)   (worker TCP listener)
//	new -> old: 'C' once the new worker serves on the start files
//	old -> new: 'C' + SCM_RIGHTS(commit files)  (fixed lifecycle, workloads, handover listeners)
//	new -> old: 'A' after it holds them; the old generation then stops
//	            accepting and drains for at most runtimewire.DrainLimit.
//
// A new generation that fails before 'A' leaves the old one serving.

const maxHandoverFiles = 4

// HandoverConfig names the descriptors sent in each phase. ServeHandover
// never closes them; a failed attempt sends the same ones to the next.
type HandoverConfig struct {
	UID    int
	Token  []byte
	Start  []*os.File
	Commit []*os.File

	peerUID func(*net.UnixConn) (int, error)
}

// ServeHandover answers new generations until one completes the handover and
// returns nil then. It never closes listener: the caller owns it and, after
// commit, the next generation serves the same socket.
func ServeHandover(ctx context.Context, listener *net.UnixListener, config HandoverConfig) error {
	if listener == nil || len(config.Token) != installationTokenBytes || len(config.Start) > maxHandoverFiles || len(config.Commit) == 0 || len(config.Commit) > maxHandoverFiles {
		return ErrSocketConfig
	}
	if config.peerUID == nil {
		config.peerUID = peerUID
	}
	stop := context.AfterFunc(ctx, func() { _ = listener.SetDeadline(time.Now()) })
	defer stop()
	for {
		connection, err := listener.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return causedFailure{message: "hostd handover socket accept failed", cause: err}
		}
		err = serveHandoverOne(connection, config)
		_ = connection.Close()
		if err == nil {
			return nil
		}
		// A failed new generation leaves this one serving; the next attempt reconnects.
	}
}

func serveHandoverOne(connection *net.UnixConn, config HandoverConfig) error {
	if uid, err := config.peerUID(connection); err != nil || uid != config.UID && uid != 0 {
		return ErrUnauthorized
	}
	if err := connection.SetDeadline(time.Now().Add(handoverStepTimeout)); err != nil {
		return err
	}
	token := make([]byte, installationTokenBytes)
	if _, err := io.ReadFull(connection, token); err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(token, config.Token) != 1 {
		return ErrUnauthorized
	}
	if err := sendHandoverFiles(connection, handoverStart, config.Start); err != nil {
		return err
	}
	if err := expectHandoverByte(connection, handoverCommit, handoverCommitTimeout); err != nil {
		return err
	}
	if err := sendHandoverFiles(connection, handoverCommit, config.Commit); err != nil {
		return err
	}
	return expectHandoverByte(connection, handoverAck, handoverStepTimeout)
}

// Handover is the new generation's side of one handover connection.
type Handover struct{ connection *net.UnixConn }

// DialHandover authenticates to the old generation and returns its start
// files. ErrNoHandover means nothing listens at path.
func DialHandover(ctx context.Context, path string, uid int, token []byte, count int) (*Handover, []*os.File, error) {
	if len(token) != installationTokenBytes || count < 1 || count > maxHandoverFiles {
		return nil, nil, ErrSocketConfig
	}
	connection, err := (&net.Dialer{Timeout: handoverStepTimeout}).DialContext(ctx, "unix", path)
	if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
		return nil, nil, ErrNoHandover
	}
	if err != nil {
		return nil, nil, causedFailure{message: "hostd handover socket connection failed", cause: err}
	}
	h := &Handover{connection: connection.(*net.UnixConn)}
	files, err := h.start(uid, token, count)
	if err != nil {
		return nil, nil, errors.Join(err, h.Close())
	}
	return h, files, nil
}

func (h *Handover) start(uid int, token []byte, count int) ([]*os.File, error) {
	if peer, err := peerUID(h.connection); err != nil || peer != uid && peer != 0 {
		return nil, ErrUnauthorized
	}
	if err := h.connection.SetDeadline(time.Now().Add(handoverStepTimeout)); err != nil {
		return nil, err
	}
	if err := writeAll(h.connection, token); err != nil {
		return nil, causedFailure{message: "hostd handover token could not be written", cause: err}
	}
	return receiveHandoverFiles(h.connection, handoverStart, count)
}

// Commit tells the old generation the new one serves, takes the commit files
// and acknowledges them; the old generation retires after the ack.
func (h *Handover) Commit(count int) ([]*os.File, error) {
	if err := h.connection.SetDeadline(time.Now().Add(handoverStepTimeout)); err != nil {
		return nil, err
	}
	if err := writeAll(h.connection, []byte{handoverCommit}); err != nil {
		return nil, causedFailure{message: "hostd handover commit could not be written", cause: err}
	}
	files, err := receiveHandoverFiles(h.connection, handoverCommit, count)
	if err != nil {
		return nil, err
	}
	if err := writeAll(h.connection, []byte{handoverAck}); err != nil {
		return nil, errors.Join(causedFailure{message: "hostd handover acknowledgement could not be written", cause: err}, closeFiles(files))
	}
	return files, nil
}

func (h *Handover) Close() error { return h.connection.Close() }

func sendHandoverFiles(connection *net.UnixConn, marker byte, files []*os.File) error {
	var rights []byte
	if len(files) != 0 {
		descriptors := make([]int, len(files))
		for i, file := range files {
			// Not file.Fd(): for a net listener's File it switches the socket,
			// shared with every holder, to blocking mode.
			raw, err := file.SyscallConn()
			if err != nil {
				return causedFailure{message: "hostd handover descriptor could not be read", cause: err}
			}
			if err := raw.Control(func(fd uintptr) { descriptors[i] = int(fd) }); err != nil {
				return causedFailure{message: "hostd handover descriptor could not be read", cause: err}
			}
		}
		rights = unix.UnixRights(descriptors...)
	}
	_, _, err := connection.WriteMsgUnix([]byte{marker}, rights, nil)
	if err != nil {
		return causedFailure{message: "hostd handover descriptors could not be sent", cause: err}
	}
	return nil
}

func receiveHandoverFiles(connection *net.UnixConn, marker byte, count int) ([]*os.File, error) {
	buffer := make([]byte, 1)
	oob := make([]byte, unix.CmsgSpace(4*maxHandoverFiles))
	n, oobn, flags, _, err := connection.ReadMsgUnix(buffer, oob)
	if err != nil {
		return nil, causedFailure{message: "hostd handover descriptors could not be received", cause: err}
	}
	messages, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return nil, causedFailure{message: "hostd handover descriptors could not be parsed", cause: err}
	}
	var files []*os.File
	for _, message := range messages {
		descriptors, err := unix.ParseUnixRights(&message)
		if err != nil {
			continue
		}
		for _, descriptor := range descriptors {
			unix.CloseOnExec(descriptor)
			files = append(files, os.NewFile(uintptr(descriptor), "hostd-handover"))
		}
	}
	if n != 1 || buffer[0] != marker || flags&unix.MSG_CTRUNC != 0 || len(files) != count {
		return nil, errors.Join(ErrInvalidFrame, closeFiles(files))
	}
	return files, nil
}

func closeFiles(files []*os.File) error {
	var result error
	for _, file := range files {
		result = errors.Join(result, file.Close())
	}
	return result
}
