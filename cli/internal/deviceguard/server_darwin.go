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

const DefaultSocket = "/var/run/paperboat-deviceguard/control.sock"
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
	if err != nil || !validAddressInCIDR(host, loopbackCIDR) {
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
	connection.SetReadDeadline(time.Now().Add(5 * time.Second))
	marker := make([]byte, 1)
	oob := make([]byte, unix.CmsgSpace(4))
	_, oobn, flags, _, err := connection.(*net.UnixConn).ReadMsgUnix(marker, oob)
	if err != nil || marker[0] != 1 || flags&unix.MSG_CTRUNC != 0 {
		return nil, errors.New("protected receiver did not return a listener")
	}
	messages, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return nil, err
	}
	var descriptors []int
	for _, message := range messages {
		fds, e := unix.ParseUnixRights(&message)
		if e != nil {
			return nil, e
		}
		descriptors = append(descriptors, fds...)
	}
	defer func() {
		for _, fd := range descriptors {
			unix.Close(fd)
		}
	}()
	if len(descriptors) != 1 {
		return nil, errors.New("invalid protected listener descriptor")
	}
	file := os.NewFile(uintptr(descriptors[0]), "protected-listener")
	listener, err := net.FileListener(file)
	// FileListener duplicates its descriptor; exactly one close owns this copy.
	file.Close()
	descriptors = nil
	return listener, err
}

// ServeSocketChild only creates a socket under the dedicated receiver UID. The
// root controller supplies an inherited private channel; it never accepts user
// control connections or receives account credentials here.
func ServeSocketChild(ctx context.Context, address, loopbackCIDR string) error {
	host, port, err := net.SplitHostPort(address)
	value, e := strconv.ParseUint(port, 10, 16)
	if err != nil || e != nil || value == 0 || !validAddressInCIDR(host, loopbackCIDR) {
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
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp4", address)
	if err != nil {
		return err
	}
	defer listener.Close()
	file, err := listener.(*net.TCPListener).File()
	if err != nil {
		return err
	}
	defer file.Close()
	connection.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, _, err = connection.(*net.UnixConn).WriteMsgUnix([]byte{1}, unix.UnixRights(int(file.Fd())), nil)
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
