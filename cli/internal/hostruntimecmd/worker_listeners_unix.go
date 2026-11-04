//go:build darwin || linux

package hostruntimecmd

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	hostruntime "github.com/pinksaucepasta/paperboat/internal/hostruntime/runtime"
)

// hostdListeners are the sockets one hostd generation serves. The fixed ones
// (lifecycle, workloads, handover) and the workers' loopback runtime listener
// pass to the next generation over the handover socket; private is this
// generation's own lifecycle socket for its workers.
type hostdListeners struct {
	socket    string
	runtime   *os.File
	private   *net.UnixListener
	lifecycle *net.UnixListener
	workloads *net.UnixListener
	handover  *net.UnixListener
	fixed     []*os.File
}

// dialHandover takes the runtime listener from the generation named by
// PAPERBOAT_HOSTD_HANDOVER_SOCKET. Nothing answering there means there is no
// running generation, and this one binds fresh sockets.
func (l *hostdListeners) dialHandover(ctx context.Context, token []byte) (*hostdproto.Handover, error) {
	path := os.Getenv(hostdproto.HandoverEnv)
	if path == "" {
		return nil, nil
	}
	if path != hostdproto.HandoverPath(l.socket) {
		return nil, errors.New("the hostd handover socket must be the lifecycle socket path plus .handover")
	}
	handover, files, err := hostdproto.DialHandover(ctx, path, os.Geteuid(), token, 1)
	if errors.Is(err, hostdproto.ErrNoHandover) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	l.runtime = files[0]
	return handover, nil
}

// bindRuntime binds the loopback listener every worker inherits, or checks the
// inherited one is bound to the runtime address.
func (l *hostdListeners) bindRuntime(host hostdHost) error {
	addresser, ok := host.(interface{ RuntimeAddress() string })
	if !ok {
		return nil
	}
	address := addresser.RuntimeAddress()
	if !hostruntime.LoopbackAddress(address) {
		return runtimeFailure("hostd runtime address is not a loopback address")
	}
	if l.runtime != nil {
		listener, err := net.FileListener(l.runtime)
		if err != nil {
			return runtimeFailure("the inherited runtime listener is not a listening socket", err)
		}
		bound := listener.Addr().String()
		if err := listener.Close(); err != nil {
			return runtimeFailure("the inherited runtime listener check could not finish", err)
		}
		if bound != address {
			return runtimeFailure("the inherited runtime listener is bound to " + bound + ", not " + address)
		}
		return nil
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return runtimeFailure("hostd could not bind the runtime address "+address, err)
	}
	l.runtime, err = listenerFile(listener.(*net.TCPListener))
	return errors.Join(err, listener.Close())
}

// bindPrivate binds this generation's worker socket and, without a handover,
// the fixed sockets too. A fresh start also removes private sockets left by
// generations that died without exiting.
func (l *hostdListeners) bindPrivate(server *hostdproto.Server, private string, fresh bool) (err error) {
	if fresh {
		removeStalePrivateSockets(l.socket)
	}
	if l.private, err = server.Listen(private); err != nil || !fresh {
		return err
	}
	if l.lifecycle, err = server.Listen(l.socket); err != nil {
		return err
	}
	workloads, err := hostdproto.ListenWorkloads(l.socket + ".workloads")
	if err != nil {
		return err
	}
	l.workloads = workloads.(*net.UnixListener)
	l.handover, err = server.Listen(hostdproto.HandoverPath(l.socket))
	return err
}

// shortcut: only crashed generations' private sockets are swept, and only on a
// fresh start; a handover start leaves them until the next fresh start.
func removeStalePrivateSockets(socket string) {
	matches, _ := filepath.Glob(socket + ".[0-9]*")
	for _, match := range matches {
		if info, err := os.Lstat(match); err == nil && info.Mode()&os.ModeSocket != 0 {
			_ = os.Remove(match)
		}
	}
}

// commit takes the fixed listeners from the old generation, checking each is
// bound to its expected path.
func (l *hostdListeners) commit(handover *hostdproto.Handover) error {
	files, err := handover.Commit(3)
	if err != nil {
		return err
	}
	defer closeHandoverFiles(files)
	expected := []string{l.socket, l.socket + ".workloads", hostdproto.HandoverPath(l.socket)}
	adopted := make([]*net.UnixListener, len(files))
	for i, file := range files {
		listener, err := net.FileListener(file)
		unix, ok := listener.(*net.UnixListener)
		if err != nil || !ok || unix.Addr().String() != expected[i] {
			if listener != nil {
				_ = listener.Close()
			}
			return runtimeFailure("the inherited listener for "+expected[i]+" is not that socket", err)
		}
		// Like a freshly bound socket, remove the path on a normal exit.
		unix.SetUnlinkOnClose(true)
		adopted[i] = unix
	}
	l.lifecycle, l.workloads, l.handover = adopted[0], adopted[1], adopted[2]
	return nil
}

func (l *hostdListeners) serveFixed(serve func(*net.UnixListener) chan error, owner *hostruntime.ProcessOwner) {
	serve(l.lifecycle)
	if owner != nil {
		owner.ServeSharedWorkloads(l.workloads)
	}
}

func (l *hostdListeners) startFiles() []*os.File {
	if l.runtime == nil {
		return nil
	}
	return []*os.File{l.runtime}
}

// commitFiles are descriptors of the fixed listeners, made once and closed
// with the listeners.
func (l *hostdListeners) commitFiles() []*os.File {
	for _, listener := range []*net.UnixListener{l.lifecycle, l.workloads, l.handover} {
		file, err := listenerFile(listener)
		if err != nil {
			// ServeHandover refuses to start a handover without all three.
			return nil
		}
		l.fixed = append(l.fixed, file)
	}
	return l.fixed
}

// stopAccepting closes this generation's copies of the fixed listeners
// without removing the paths the next generation now serves. Connections
// already accepted keep running.
func (l *hostdListeners) stopAccepting() {
	for _, listener := range []*net.UnixListener{l.lifecycle, l.workloads, l.handover} {
		listener.SetUnlinkOnClose(false)
		_ = listener.Close()
	}
}

func (l *hostdListeners) close() error {
	var result error
	for _, listener := range []*net.UnixListener{l.private, l.lifecycle, l.workloads, l.handover} {
		if listener != nil {
			if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				result = errors.Join(result, err)
			}
		}
	}
	if l.runtime != nil {
		result = errors.Join(result, l.runtime.Close())
	}
	return errors.Join(result, closeHandoverFiles(l.fixed))
}

// listenerFile duplicates a listener's descriptor without TCPListener.File,
// whose Fd (called by os/exec and SCM_RIGHTS sends) would switch the socket,
// shared with every process holding it, to blocking mode.
func listenerFile(listener syscall.Conn) (*os.File, error) {
	raw, err := listener.SyscallConn()
	if err != nil {
		return nil, err
	}
	duplicate := -1
	var dupErr error
	if err := raw.Control(func(fd uintptr) { duplicate, dupErr = unix.FcntlInt(fd, unix.F_DUPFD_CLOEXEC, 0) }); err != nil {
		return nil, err
	}
	if dupErr != nil {
		return nil, dupErr
	}
	return os.NewFile(uintptr(duplicate), "hostd-listener"), nil
}

func closeHandoverFiles(files []*os.File) error {
	var result error
	for _, file := range files {
		result = errors.Join(result, file.Close())
	}
	return result
}
