//go:build darwin

package deviceguard

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/deviceloopback"
	"golang.org/x/sys/unix"
)

const DefaultSocket = "/Library/Application Support/Paperboat/deviceguard-control/control.sock"
const DefaultStateDir = "/Library/Application Support/Paperboat/deviceguard"
const defaultDNSPort = "53535"
const defaultDNSAddress = "127.100.0.1:" + defaultDNSPort
const darwinBrokerAccount = "_paperboat_deviceguard"

// The receiver must have its own OS identity: permitting all root sockets would
// admit unrelated privileged wildcard listeners after the controller exits.
var darwinBrokerIdentity = func(ctx context.Context) (uint32, uint32, error) {
	var ids [2]uint32
	for i, flag := range []string{"-u", "-g"} {
		output, err := exec.CommandContext(ctx, "/usr/bin/id", flag, darwinBrokerAccount).Output()
		if err != nil {
			return 0, 0, errors.New("Paperboat device guard account is missing; run device-guard install")
		}
		value, err := strconv.ParseUint(strings.TrimSpace(string(output)), 10, 32)
		if err != nil || value == 0 {
			return 0, 0, errors.New("invalid device guard receiver identity")
		}
		ids[i] = uint32(value)
	}
	return ids[0], ids[1], nil
}

var darwinSocketCommand = func(ctx context.Context, address, loopbackCIDR string) (*exec.Cmd, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	return exec.CommandContext(ctx, executable, "daemon", "device-guard", "socket", address, loopbackCIDR), nil
}

func peerUID(conn *net.UnixConn) (uint32, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Xucred
	var optionErr error
	err = raw.Control(func(fd uintptr) {
		cred, optionErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	})
	if err != nil || optionErr != nil || cred == nil {
		return 0, errors.Join(err, optionErr, errors.New("cannot authenticate local guard peer"))
	}
	return cred.Uid, nil
}

func listenProtected(ctx context.Context, address, loopbackCIDR string) (net.Listener, error) {
	uid, gid, err := darwinBrokerIdentity(ctx)
	if err != nil {
		return nil, err
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil || !validProtectedBindAddress(address, loopbackCIDR) {
		return nil, errors.New("invalid protected listener address")
	}
	if err = darwinEnsureAlias(ctx, host); err != nil {
		return nil, err
	}
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, err
	}
	parent := os.NewFile(uintptr(pair[0]), "guard-listener-parent")
	child := os.NewFile(uintptr(pair[1]), "guard-listener-child")
	defer parent.Close()
	defer child.Close()
	connection, err := net.FileConn(parent)
	if err != nil {
		return nil, err
	}
	defer connection.Close()
	command, err := darwinSocketCommand(ctx, address, loopbackCIDR)
	if err != nil {
		return nil, err
	}
	command.ExtraFiles = []*os.File{child}
	command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: gid}}
	// Never give the broker child user credentials or an inherited agent socket.
	command.Env = []string{"PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	if err = command.Start(); err != nil {
		return nil, err
	}
	child.Close()
	wait := make(chan error, 1)
	go func() { wait <- command.Wait() }()
	defer func() { _ = command.Process.Kill(); <-wait }()
	deadline := time.Now().Add(5 * time.Second)
	if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	if err := connection.SetReadDeadline(deadline); err != nil {
		return nil, err
	}
	stopRead := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stopRead()
	fd, err := receiveDarwinSocket(connection.(*net.UnixConn))
	if err != nil {
		return nil, err
	}
	return bindDarwinSocket(ctx, fd, address, loopbackCIDR)
}

// receiveDarwinSocket owns every descriptor delivered by this private channel,
// including descriptors accompanying a malformed response.
func receiveDarwinSocket(connection *net.UnixConn) (int, error) {
	marker := make([]byte, 2)
	oob := make([]byte, unix.CmsgSpace(4*16))
	n, oobn, flags, _, readErr := connection.ReadMsgUnix(marker, oob)
	messages, parseErr := unix.ParseSocketControlMessage(oob[:oobn])
	var descriptors []int
	valid := true
	for _, message := range messages {
		fds, err := unix.ParseUnixRights(&message)
		if err != nil {
			valid = false
			continue
		}
		descriptors = append(descriptors, fds...)
	}
	if readErr != nil || parseErr != nil || !valid || n != 1 || marker[0] != 1 || flags&(unix.MSG_CTRUNC|unix.MSG_TRUNC) != 0 || len(descriptors) != 1 {
		for _, fd := range descriptors {
			_ = unix.Close(fd)
		}
		return -1, errors.New("protected receiver did not return one socket")
	}
	unix.CloseOnExec(descriptors[0])
	return descriptors[0], nil
}

// The socket's creation credentials remain the broker's. XNU checks the
// calling process credentials at bind, so only this root controller performs
// privileged-port binding. PF continues to identify the broker-owned socket.
func bindDarwinSocket(ctx context.Context, fd int, address, loopbackCIDR string) (net.Listener, error) {
	file := os.NewFile(uintptr(fd), "protected-socket")
	if file == nil {
		return nil, errors.New("invalid protected socket")
	}
	defer file.Close()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validProtectedBindAddress(address, loopbackCIDR) {
		return nil, errors.New("invalid protected listener address")
	}
	kind, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_TYPE)
	if err != nil || kind != unix.SOCK_STREAM {
		return nil, errors.New("protected receiver returned a non-TCP socket")
	}
	if _, err = unix.GetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_NODELAY); err != nil {
		return nil, errors.New("protected receiver returned a non-TCP socket")
	}
	bound, err := unix.Getsockname(fd)
	ipv4, ok := bound.(*unix.SockaddrInet4)
	if err != nil || !ok || ipv4.Port != 0 || ipv4.Addr != [4]byte{} {
		return nil, errors.New("protected receiver returned a bound or non-IPv4 socket")
	}
	// Darwin does not expose SO_ACCEPTCONN through getsockopt. A TCP
	// listener necessarily has a bound local port, already excluded above.
	host, portText, _ := net.SplitHostPort(address)
	port, portErr := strconv.Atoi(portText)
	if portErr != nil || port < 1 || port > 65535 {
		return nil, errors.New("invalid protected port")
	}
	ip := net.ParseIP(host).To4()
	if ip == nil {
		return nil, errors.New("invalid protected IPv4 address")
	}
	target := &unix.SockaddrInet4{Port: port}
	copy(target.Addr[:], ip)
	if err = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err != nil {
		return nil, err
	}
	if err = unix.Bind(fd, target); err != nil {
		return nil, classifyProtectedBindError(err)
	}
	if err = unix.Listen(fd, unix.SOMAXCONN); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return net.FileListener(file)
}

// ServeSocketChild never regains root: it creates one unbound IPv4 TCP socket
// under the dedicated receiver UID and hands it to the inherited private channel.
func ServeSocketChild(ctx context.Context, address, loopbackCIDR string) error {
	_, rawPort, splitErr := net.SplitHostPort(address)
	port, portErr := strconv.Atoi(rawPort)
	if splitErr != nil || portErr != nil || port < 1 || port > 65535 {
		return errors.New("invalid protected port")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validProtectedBindAddress(address, loopbackCIDR) {
		return errors.New("invalid protected listener")
	}
	channel := os.NewFile(3, "guard-parent")
	if channel == nil {
		return errors.New("missing guard parent")
	}
	defer channel.Close()
	connection, err := net.FileConn(channel)
	if err != nil {
		return err
	}
	defer connection.Close()
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, unix.IPPROTO_TCP)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	unix.CloseOnExec(fd)
	if err = connection.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	n, _, err := connection.(*net.UnixConn).WriteMsgUnix([]byte{1}, unix.UnixRights(fd), nil)
	if err == nil && n != 1 {
		return errors.New("incomplete protected socket handoff")
	}
	return err
}

func shutdownListener(listener net.Listener) error {
	socket, ok := listener.(*net.TCPListener)
	if !ok {
		return listener.Close()
	}
	raw, err := socket.SyscallConn()
	var shutdownErr error
	if err == nil {
		err = raw.Control(func(fd uintptr) { shutdownErr = unix.Shutdown(int(fd), unix.SHUT_RDWR) })
	}
	if errors.Is(shutdownErr, unix.ENOTCONN) {
		shutdownErr = nil
	}
	return errors.Join(err, shutdownErr, listener.Close())
}

func notifyReady() error { return nil }

var darwinAnchor = "com.apple/000.paperboat-deviceguard"
var darwinPrefix = ""

func darwinRules(ctx context.Context, leases []*guardedLease, dnsAddress string, loopbackCIDR ...string) (string, error) {
	broker, _, err := darwinBrokerIdentity(ctx)
	if err != nil {
		return "", err
	}
	host, port, err := net.SplitHostPort(dnsAddress)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	// No remote interface may enter a loopback projection. DNS is served only by
	// the protected controller and carries names, never private service payloads.
	prefixes := loopbackCIDR
	if darwinPrefix != "" {
		prefixes = []string{darwinPrefix}
	}
	if len(prefixes) == 0 {
		prefixes = []string{deviceloopback.DefaultCIDR}
	}
	for _, prefix := range prefixes {
		fmt.Fprintf(&b, "block drop quick on ! lo0 inet from any to %s\n", prefix)
	}
	for _, direction := range []string{"out", "in"} {
		fmt.Fprintf(&b, "pass %s quick on lo0 inet proto { tcp udp } from any to %s port %s no state\n", direction, host, port)
		fmt.Fprintf(&b, "pass %s quick on lo0 inet proto { tcp udp } from %s port %s to any no state\n", direction, host, port)
	}
	for _, lease := range leases {
		uid, err := strconv.ParseUint(lease.uid, 10, 32)
		if err != nil {
			return "", errors.New("invalid caller UID")
		}
		fmt.Fprintf(&b, "pass out quick on lo0 inet proto tcp from any to %s port %d user %d no state\n", lease.ip, lease.port, uid)
		fmt.Fprintf(&b, "pass out quick on lo0 inet proto tcp from %s port %d to any user %d no state\n", lease.ip, lease.port, broker)
		fmt.Fprintf(&b, "pass in quick on lo0 inet proto tcp from any to %s port %d user %d flags S/SA keep state\n", lease.ip, lease.port, broker)
		fmt.Fprintf(&b, "pass in quick on lo0 inet proto tcp from %s port %d to any user %d no state\n", lease.ip, lease.port, uid)
	}
	for _, prefix := range prefixes {
		fmt.Fprintf(&b, "block drop out quick inet from any to %s\nblock drop out quick inet from %s to any\nblock drop in quick inet from any to %s\nblock drop in quick inet from %s to any\n", prefix, prefix, prefix, prefix)
	}
	return b.String(), nil
}

func darwinRun(ctx context.Context, input string, program string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, program, args...)
	if input != "" {
		command.Stdin = strings.NewReader(input)
	}
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("device guard %s failed: %w: %s", filepath.Base(program), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

func darwinWrite(path string, data []byte, mode os.FileMode) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".paperboat-")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if err = file.Chmod(mode); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
