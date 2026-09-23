//go:build linux

package deviceguard

import (
	"context"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/internal/deviceloopback"
	"golang.org/x/sys/unix"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const DefaultSocket = "/run/paperboat-deviceguard/control.sock"
const DefaultStateDir = "/var/lib/paperboat-deviceguard"
const defaultDNSPort = "53535"
const defaultDNSAddress = "127.100.0.1:" + defaultDNSPort
const guardMark = 0x5042
const guardTable = "paperboat_deviceguard"

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
	return config.Listen(ctx, "tcp4", address)
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
func configureDomains(ctx context.Context, cfg Config, domains []string) error {
	routing := make([]string, len(domains))
	for i, domain := range domains {
		routing[i] = "~" + domain
	}
	if len(routing) == 0 {
		routing = []string{""}
	}
	args := append([]string{"domain", "paperboat-dns"}, routing...)
	return exec.CommandContext(ctx, "resolvectl", args...).Run()
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
	dnsHost, _, err := net.SplitHostPort(cfg.DNSAddress)
	if err != nil {
		return errors.New("invalid device guard DNS address")
	}
	for _, cidr := range protectedLoopbackCIDRs(cfg) {
		fmt.Fprintf(&script, "ip daddr %s ip daddr != %s reject\n", cidr, dnsHost)
	}
	script.WriteString("}\nchain input { type filter hook input priority -10; policy accept;\n")
	for _, cidr := range protectedLoopbackCIDRs(cfg) {
		fmt.Fprintf(&script, "iifname != \"lo\" ip daddr %s reject\n", cidr)
	}
	for _, l := range leases {
		fmt.Fprintf(&script, "ip daddr %s tcp dport %d tcp flags & (syn | ack) == syn socket mark %d ct mark set %d accept\n", l.ip, l.port, guardMark, guardMark)
		fmt.Fprintf(&script, "ip daddr %s tcp dport %d tcp flags & syn == 0 ct mark %d ct state established,related accept\n", l.ip, l.port, guardMark)
	}
	for _, cidr := range protectedLoopbackCIDRs(cfg) {
		fmt.Fprintf(&script, "ip daddr %s ip daddr != %s reject\n", cidr, dnsHost)
	}
	script.WriteString("}\n}\n")
	command := exec.CommandContext(ctx, "nft", "-f", "-")
	command.Stdin = strings.NewReader(script.String())
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("install protected listener rules: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}
func setupResolver(ctx context.Context, cfg Config) error {
	if _, err := exec.LookPath("resolvectl"); err != nil {
		return err
	}

	if err := exec.CommandContext(ctx, "ip", "link", "show", "paperboat-dns").Run(); err == nil {
		alias, readErr := os.ReadFile("/sys/class/net/paperboat-dns/ifalias")
		if readErr != nil || strings.TrimSpace(string(alias)) != "paperboat-deviceguard-v1" {
			return errors.New("paperboat-dns link already exists without Paperboat ownership")
		}
	} else if err := exec.CommandContext(ctx, "ip", "link", "add", "name", "paperboat-dns", "alias", "paperboat-deviceguard-v1", "type", "dummy").Run(); err != nil {
		return err
	}
	if err := exec.CommandContext(ctx, "ip", "link", "set", "paperboat-dns", "up").Run(); err != nil {
		retireResolver(cfg)
		return err
	}
	dnsHost, _, err := net.SplitHostPort(cfg.DNSAddress)
	if err != nil {
		return err
	}
	if out, e := exec.CommandContext(ctx, "ip", "address", "replace", dnsHost+"/32", "dev", "paperboat-dns", "scope", "global").CombinedOutput(); e != nil {
		err = fmt.Errorf("configure private DNS link address: %w: %s", e, out)
		retireResolver(cfg)
		return err
	}
	for _, args := range [][]string{{"dns", "paperboat-dns", cfg.DNSAddress}, {"default-route", "paperboat-dns", "no"}} {
		if out, err := exec.CommandContext(ctx, "resolvectl", args...).CombinedOutput(); err != nil {
			retireResolver(cfg)
			return fmt.Errorf("configure private DNS resolver: %w: %s", err, out)
		}
	}
	// An abrupt helper exit leaves systemd-resolved's per-link routing domains
	// behind. Retire them before reporting readiness; the next authoritative
	// name snapshot will publish the domains that still have protected listeners.
	if err := configureDomains(ctx, cfg, nil); err != nil {
		retireResolver(cfg)
		return fmt.Errorf("clear stale private DNS routing domains: %w", err)
	}
	return nil
}
func retireResolver(cfg Config) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = removeOwnedResolver(ctx, cfg)
}

func removeOwnedResolver(ctx context.Context, _ Config) error {
	if err := exec.CommandContext(ctx, "ip", "link", "show", "paperboat-dns").Run(); err != nil {
		return nil
	}
	alias, err := os.ReadFile("/sys/class/net/paperboat-dns/ifalias")
	if err != nil || strings.TrimSpace(string(alias)) != "paperboat-deviceguard-v1" {
		return errors.New("paperboat-dns link exists without Paperboat ownership")
	}
	if output, err := exec.CommandContext(ctx, "resolvectl", "revert", "paperboat-dns").CombinedOutput(); err != nil {
		return fmt.Errorf("retire private DNS resolver: %w: %s", err, strings.TrimSpace(string(output)))
	}
	if output, err := exec.CommandContext(ctx, "ip", "link", "delete", "paperboat-dns").CombinedOutput(); err != nil {
		return fmt.Errorf("delete private DNS link: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func shutdownListener(listener net.Listener) error {
	tcp, ok := listener.(*net.TCPListener)
	if !ok {
		return errors.New("device guard listener lost kernel ownership")
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
	dnsAddress, _ := deviceloopback.DNSAddress(state.Active)
	return applyProtection(ctx, Config{LoopbackCIDR: state.Active, ProtectedLoopbackCIDRs: state.Protected, DNSAddress: net.JoinHostPort(dnsAddress.String(), defaultDNSPort)}, nil)
}
