//go:build darwin

package machineguard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/cli/internal/machineloopback"
	"github.com/pinksaucepasta/paperboat/cli/internal/splitdns"
	wire "github.com/pinksaucepasta/paperboat/shared/managedssh"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

var darwinPlatform struct {
	sync.Mutex
	cfg      Config
	aliases  map[string]bool
	retiring bool            // darwinRetireLater is scheduled
	active   map[string]bool // addresses the last applied policy serves
}

func darwinLoadAliases(cfg Config) error {
	if darwinPlatform.aliases != nil && darwinPlatform.cfg.StateDir == cfg.StateDir {
		return nil
	}
	darwinPlatform.cfg = cfg
	darwinPlatform.aliases = map[string]bool{}
	data, err := os.ReadFile(filepath.Join(cfg.StateDir, "aliases.json"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(data) > 1<<20 || json.Unmarshal(data, &darwinPlatform.aliases) != nil {
		return errors.New("invalid Paperboat alias journal")
	}
	for address := range darwinPlatform.aliases {
		if !validOwnedLoopbackAddress(address) && address != splitdns.BrowserGatewayIP && !sshSourceAddress(address) {
			return errors.New("invalid protected alias journal address")
		}
	}
	return nil
}
func darwinSaveAliases() error {
	data, err := json.Marshal(darwinPlatform.aliases)
	if err != nil {
		return err
	}
	return darwinWrite(filepath.Join(darwinPlatform.cfg.StateDir, "aliases.json"), data, 0600)
}

// darwinAliasExists reports whether address is configured: only then can a socket bind it. Listing lo0's addresses
// per listener (ifconfig, or the routing socket) grows with the machines.
func darwinAliasExists(address string) (bool, error) {
	probe, err := net.ListenPacket("udp4", net.JoinHostPort(address, "0"))
	if errors.Is(err, syscall.EADDRNOTAVAIL) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, probe.Close()
}
func darwinEnsureAlias(ctx context.Context, address string) error {
	darwinPlatform.Lock()
	defer darwinPlatform.Unlock()
	return darwinEnsureAliasLocked(ctx, address)
}
func darwinEnsureAliasLocked(ctx context.Context, address string) error {
	// Canonical loopback already exists and is never task-owned.
	if address == "127.0.0.1" {
		return nil
	}
	exists, err := darwinAliasExists(address)
	if err != nil {
		return err
	}
	if exists && !darwinPlatform.aliases[address] {
		return errors.New("protected address is configured by another owner")
	}
	if darwinPlatform.active != nil {
		darwinPlatform.active[address] = true // being acquired: a background retire leaves it alone
	}
	if exists {
		return nil
	}
	darwinPlatform.aliases[address] = true
	// Journal before mutation: restart can clean up even after process death.
	if err = darwinSaveAliases(); err != nil {
		return err
	}
	_, err = darwinRun(ctx, "", "/sbin/ifconfig", "lo0", "alias", address, "netmask", "255.255.255.255")
	return err
}

func applyProtection(ctx context.Context, cfg Config, leases []*guardedLease) error {
	darwinPlatform.Lock()
	defer darwinPlatform.Unlock()
	if err := darwinLoadAliases(cfg); err != nil {
		return darwinStep("read the alias journal", err)
	}
	needHook, err := darwinValidatePolicy(ctx)
	if err != nil {
		return darwinStep("check the PF root ruleset", err)
	}
	rules, err := darwinRules(ctx, leases, protectedLoopbackCIDRs()...)
	if err != nil {
		return darwinStep("build the PF rules", err)
	}
	if _, err = darwinRun(ctx, rules, "/sbin/pfctl", "-a", darwinAnchor, "-f", "-"); err != nil {
		return darwinStep("load the Paperboat PF anchor", err)
	}
	if needHook {
		if err = darwinInstallHook(ctx); err != nil {
			return darwinStep("add the Paperboat anchor to the PF root ruleset", err)
		}
	}
	if err = darwinEnsureRdrHook(ctx); err != nil {
		return darwinStep("add the Paperboat anchor to the PF translation rules", err)
	}
	status, err := darwinRun(ctx, "", "/sbin/pfctl", "-s", "info")
	if err != nil {
		return darwinStep("read the PF status", err)
	}
	if !strings.Contains(string(status), "Status: Enabled") {
		output, err := darwinRun(ctx, "", "/sbin/pfctl", "-E")
		if err != nil {
			return darwinStep("enable PF", err)
		}
		token := ""
		for _, line := range strings.Split(string(output), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "Token :") {
				token = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "Token :"))
			}
		}
		if token == "" {
			return darwinStep("enable PF", errors.New("PF enable reference was not returned"))
		}
		if err = darwinWrite(filepath.Join(cfg.StateDir, "pf-enable-token"), []byte(token), 0600); err != nil {
			return darwinStep("record the PF enable reference", err)
		}
	}
	active := map[string]bool{}
	for _, lease := range leases {
		active[lease.ip] = true
	}
	if err = darwinEnsureSSHSourcesLocked(ctx, active); err != nil {
		return darwinStep("add the managed SSH source addresses", err)
	}
	darwinPlatform.active = active
	return darwinRetireLocked(ctx, len(leases) == 0)
}

const (
	darwinRetireConcurrency = 16
	// darwinRetireBatch bounds the addresses one apply retires: a daemon that stops retires all its machines at once,
	// and 1000 of them outlasted the deadline and stopped the guard. The PF rules already deny the rest.
	darwinRetireBatch = 64
)

// darwinRetireLocked ends the addresses without a listener, a batch now and the others in the background.
func darwinRetireLocked(ctx context.Context, all bool) error {
	var retired []string
	for address := range darwinPlatform.aliases {
		if !darwinPlatform.active[address] {
			retired = append(retired, address)
		}
	}
	if batch := darwinRetireBatch; len(retired) > batch {
		if all { // no listener is left: end every machine connection's state at once, and remove the addresses later
			if _, err := darwinRun(ctx, "", "/sbin/pfctl", "-k", "0.0.0.0/0", "-k", machineloopback.DefaultCIDR); err != nil {
				return err
			}
			batch = 0
		}
		retired = retired[:batch]
		if !darwinPlatform.retiring {
			darwinPlatform.retiring = true
			time.AfterFunc(time.Second, darwinRetireLater)
		}
	}
	return darwinRetireAddresses(ctx, retired)
}

// darwinRetireAddresses requires darwinPlatform's lock.
func darwinRetireAddresses(ctx context.Context, retired []string) error {
	results := make([]error, len(retired))
	var wg sync.WaitGroup
	slots := make(chan struct{}, darwinRetireConcurrency)
	for i, address := range retired {
		slots <- struct{}{}
		wg.Go(func() {
			defer func() { <-slots }()
			results[i] = darwinRetireAddress(ctx, address)
		})
	}
	wg.Wait()
	for i, address := range retired {
		if results[i] == nil {
			delete(darwinPlatform.aliases, address)
		}
	}
	return errors.Join(errors.Join(results...), darwinSaveAliases())
}

// darwinRetireLater retires one short batch, so a listener request never waits long for the lock, and continues until
// none is left. Addresses that stay are denied and journaled.
func darwinRetireLater() {
	darwinPlatform.Lock()
	defer darwinPlatform.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var retired []string
	for address := range darwinPlatform.aliases {
		if !darwinPlatform.active[address] {
			retired = append(retired, address)
		}
	}
	if len(retired) == 0 {
		darwinPlatform.retiring = false
		return
	}
	_ = darwinRetireAddresses(ctx, retired[:min(len(retired), darwinRetireConcurrency)])
	time.AfterFunc(time.Second, darwinRetireLater)
}

// darwinRetireAddress ends an address with no listener left. It kills only that address's states; never flush other
// Paperboat machines, normal loopback traffic, or system PF policy.
func darwinRetireAddress(ctx context.Context, address string) error {
	if _, err := darwinRun(ctx, "", "/sbin/pfctl", "-k", "0.0.0.0/0", "-k", address); err != nil {
		return err
	}
	exists, err := darwinAliasExists(address)
	if err != nil || !exists {
		return err
	}
	_, err = darwinRun(ctx, "", "/sbin/ifconfig", "lo0", "-alias", address)
	return err
}

// darwinEnsureSSHSourcesLocked aliases every managed SSH source address (wire.LoopbackSources) on lo0 with one
// ifconfig read, and marks them active: macOS routes only aliased 127/8 addresses, and the user's carrier binds them
// to reach sshd from one address per user.
func darwinEnsureSSHSourcesLocked(ctx context.Context, active map[string]bool) error {
	output, err := darwinRun(ctx, "", "/sbin/ifconfig", "lo0")
	if err != nil {
		return err
	}
	present := map[string]bool{}
	for _, line := range strings.Split(string(output), "\n") {
		if fields := strings.Fields(line); len(fields) >= 2 && fields[0] == "inet" {
			present[fields[1]] = true
		}
	}
	for _, source := range wire.LoopbackSources() {
		address := source.String()
		active[address] = true
		if present[address] {
			continue // ours from an earlier apply, or another owner's: either way it routes
		}
		darwinPlatform.aliases[address] = true
		if err := darwinSaveAliases(); err != nil {
			return err
		}
		if _, err := darwinRun(ctx, "", "/sbin/ifconfig", "lo0", "alias", address, "netmask", "255.255.255.255"); err != nil {
			return err
		}
	}
	return nil
}

func sshSourceAddress(address string) bool {
	parsed, err := netip.ParseAddr(address)
	return err == nil && slices.Contains(wire.LoopbackSources(), parsed)
}

func darwinStep(step string, err error) error {
	return fmt.Errorf("machine guard could not %s: %w", step, err)
}

func retirePlatform(cfg Config) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	darwinPlatform.Lock()
	defer darwinPlatform.Unlock()
	if err := darwinLoadAliases(cfg); err != nil {
		return
	}
	// All controller DNS and service sockets have stopped before this hook. macOS
	// does not route unused127/8 aliases to loopback, so deleting owned aliases is
	// an additional crash/reboot barrier independent of the retained PF denial.
	for address := range darwinPlatform.aliases {
		exists, err := darwinAliasExists(address)
		if err != nil {
			return
		}
		if exists {
			if _, err = darwinRun(ctx, "", "/sbin/ifconfig", "lo0", "-alias", address); err != nil {
				return
			}
		}
		delete(darwinPlatform.aliases, address)
	}
	if err := darwinSaveAliases(); err != nil {
		return
	}
	tokenPath := filepath.Join(cfg.StateDir, "pf-enable-token")
	if token, err := os.ReadFile(tokenPath); err == nil {
		value := strings.TrimSpace(string(token))
		if value != "" {
			if _, err = darwinRun(ctx, "", "/sbin/pfctl", "-X", value); err != nil {
				return
			}
		}
		_ = os.Remove(tokenPath)
	}
}

// PF evaluates `quick` matches in order, so Paperboat's quick rules for the protected range decide first when
// the root ruleset reaches the guard's anchor before any other quick rule. An administrator's own pf.conf and
// third-party firewalls are normal: rules AFTER the guard's anchor never see protected traffic, so they are
// accepted; a quick rule BEFORE it could bypass protection and is named in the error. A root ruleset without
// the Apple wildcard anchor gets a hook for the guard's own anchor added at its top (darwinInstallHook), and
// every other user rule stays as it is.
func darwinValidatePolicy(ctx context.Context) (needHook bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	query := func(args ...string) (string, error) {
		out, err := exec.CommandContext(ctx, "/sbin/pfctl", args...).Output()
		if err != nil {
			return "", fmt.Errorf("inspect PF protection policy: %w", err)
		}
		return string(out), nil
	}
	root, err := query("-sr")
	if err != nil {
		return false, err
	}
	interfaces, err := query("-v", "-s", "Interfaces", "-i", "lo0")
	if err != nil {
		return false, err
	}
	anchors := ""
	if hook, _, _ := parseDarwinRoot(root, darwinAnchor); hook == appleHook {
		if anchors, err = query("-a", "com.apple", "-s", "Anchors"); err != nil {
			return false, err
		}
	}
	budget := 64
	return validateDarwinPolicy(root, interfaces, anchors, darwinAnchor,
		func(anchor string) error { return darwinEarlierAnchorOK(query, anchor, &budget, 0, darwinRulesEmpty) },
		func(anchor string) error { return darwinEarlierAnchorOK(query, anchor, &budget, 0, darwinRulesNoQuick) })
}

// darwinInstallHook reloads only the root FILTER rules (-R) with the guard's anchor first: NAT, options and
// every user rule are kept as they are.
func darwinInstallHook(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, "/sbin/pfctl", "-sr").Output()
	if err != nil {
		return fmt.Errorf("read the PF root ruleset: %w", err)
	}
	_, err = darwinRun(ctx, darwinInsertHook(string(out), darwinAnchor), "/sbin/pfctl", "-R", "-f", "-")
	return err
}

// darwinEnsureRdrHook makes the root translation rules reach the anchor's rdr rules (portRedirects): macOS's
// pf.conf does with rdr-anchor "com.apple/*"; a pf.conf without it gets the guard's own rdr-anchor appended
// (-N reloads only the translation rules, every filter rule and option stays).
func darwinEnsureRdrHook(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, "/sbin/pfctl", "-s", "nat").Output()
	if err != nil {
		return fmt.Errorf("read the PF translation rules: %w", err)
	}
	if darwinReachesRdr(string(out), darwinAnchor) {
		return nil
	}
	_, err = darwinRun(ctx, strings.TrimRight(string(out), "\n")+"\n"+`rdr-anchor "`+darwinAnchor+`" all`+"\n", "/sbin/pfctl", "-N", "-f", "-")
	return err
}
