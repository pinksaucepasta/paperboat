//go:build linux

package machineguard

import (
	"context"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/internal/machineloopback"
	"golang.org/x/sys/unix"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const DefaultSocket = "/run/paperboat-machineguard/control.sock"
const DefaultStateDir = "/var/lib/paperboat-machineguard"
const guardMark = 0x5042
const guardTable = "paperboat_machineguard"

func peerUID(conn *net.UnixConn) (uint32, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Ucred
	var e error
	err = raw.Control(func(fd uintptr) { cred, e = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED) })
	if err != nil || e != nil {
		return 0, errors.Join(err, e)
	}
	return cred.Uid, nil
}
func listenProtected(ctx context.Context, address, _ string) (net.Listener, error) {
	config := net.ListenConfig{Control: func(_, _ string, raw syscall.RawConn) error {
		var optionErr error
		err := raw.Control(func(fd uintptr) { optionErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK, guardMark) })
		return errors.Join(err, optionErr)
	}}
	listener, err := config.Listen(ctx, "tcp4", address)
	return listener, classifyProtectedBindError(err)
}
func notifyReady() error {
	notify := os.Getenv("NOTIFY_SOCKET")
	if notify == "" {
		return nil
	}
	connection, err := net.Dial("unixgram", notify)
	if err != nil {
		return err
	}
	defer connection.Close()
	_, err = connection.Write([]byte("READY=1"))
	return err
}
func applyProtection(ctx context.Context, cfg Config, leases []*guardedLease) error {
	var script strings.Builder
	// nft's batch transaction swaps the entire owned table atomically.
	if err := exec.CommandContext(ctx, "nft", "list", "table", "inet", guardTable).Run(); err == nil {
		fmt.Fprintf(&script, "delete table inet %s\n", guardTable)
	}
	fmt.Fprintf(&script, "table inet %s {\nchain output { type filter hook output priority -10; policy accept;\n", guardTable)
	for _, l := range leases {
		uid, err := strconv.ParseUint(l.uid, 10, 32)
		if err != nil {
			return errors.New("invalid Unix guard owner")
		}
		fmt.Fprintf(&script, "ip daddr %s tcp dport %d meta skuid %d accept\n", l.ip, l.port, uid)
	}
	// Replies can target the protected gateway address itself. Only a marked
	// broker socket in an already admitted connection may bypass the UID check.
	fmt.Fprintf(&script, "meta mark %d ct mark %d ct state established,related accept\n", guardMark, guardMark)
	for _, cidr := range protectedLoopbackCIDRs(cfg) {
		fmt.Fprintf(&script, "ip daddr %s reject\n", cidr)
	}
	script.WriteString("}\nchain input { type filter hook input priority -10; policy accept;\n")
	for _, cidr := range protectedLoopbackCIDRs(cfg) {
		fmt.Fprintf(&script, "iifname != \"lo\" ip daddr %s reject\n", cidr)
	}
	for _, l := range leases {
		fmt.Fprintf(&script, "ip daddr %s tcp dport %d tcp flags & (syn | ack) == syn socket mark %d ct mark set %d accept\n", l.ip, l.port, guardMark, guardMark)
		fmt.Fprintf(&script, "ip daddr %s tcp dport %d tcp flags & syn == 0 ct mark %d ct state established,related accept\n", l.ip, l.port, guardMark)
	}
	// Return traffic inherits the connection mark installed on an owned SYN.
	fmt.Fprintf(&script, "ct mark %d ct state established,related accept\n", guardMark)
	for _, cidr := range protectedLoopbackCIDRs(cfg) {
		fmt.Fprintf(&script, "ip daddr %s reject\n", cidr)
	}
	script.WriteString("}\n}\n")
	command := exec.CommandContext(ctx, "nft", "-f", "-")
	command.Stdin = strings.NewReader(script.String())
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("install protected listener rules: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}
func cleanupHistoricalLocalNames(ctx context.Context, cfg Config) error {
	if err := clearMachineHosts(ctx, cfg); err != nil {
		return err
	}
	linkPath := cfg.legacyDNSInterfacePath
	if linkPath == "" {
		linkPath = "/sys/class/net/paperboat-dns"
	}
	return cleanupLinuxLegacyDNSLink(ctx, linkPath, func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, name, args...).CombinedOutput()
	})
}

func cleanupLinuxLegacyDNSLink(ctx context.Context, linkPath string, run func(context.Context, string, ...string) ([]byte, error)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := os.Stat(linkPath); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	alias, err := os.ReadFile(filepath.Join(linkPath, "ifalias"))
	if err != nil || strings.TrimSpace(string(alias)) != "paperboat-machineguard-v1" {
		return errors.New("paperboat-dns link exists without Paperboat ownership")
	}
	if output, err := run(ctx, "resolvectl", "revert", "paperboat-dns"); err != nil {
		return fmt.Errorf("retire private DNS resolver: %w: %s", err, strings.TrimSpace(string(output)))
	}
	if output, err := run(ctx, "ip", "link", "delete", "paperboat-dns"); err != nil {
		return fmt.Errorf("delete private DNS link: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func shutdownListener(listener net.Listener) error {
	tcp, ok := listener.(*net.TCPListener)
	if !ok {
		return errors.New("machine guard listener lost kernel ownership")
	}
	raw, err := tcp.SyscallConn()
	if err != nil {
		return err
	}
	var optionErr, shutdownErr error
	err = raw.Control(func(fd uintptr) {
		optionErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_MARK, 0)
		shutdownErr = unix.Shutdown(int(fd), unix.SHUT_RDWR)
	})
	return errors.Join(err, optionErr, shutdownErr)
}

func retirePlatform(cfg Config) {}

// RestoreDeny restores only the persistent prefix boundary before local logins.
func RestoreDeny(ctx context.Context) error {
	if err := requirePrivilege(); err != nil {
		return err
	}
	state, err := loadLoopbackState(DefaultStateDir)
	if err != nil {
		return err
	}
	return applyProtection(ctx, Config{LoopbackCIDR: machineloopback.DefaultCIDR, ProtectedLoopbackCIDRs: state.Protected}, nil)
}
