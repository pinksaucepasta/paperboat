//go:build darwin

package machineguard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/internal/splitdns"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var darwinPlatform struct {
	sync.Mutex
	cfg     Config
	aliases map[string]bool
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
		if !validOwnedLoopbackAddress(address, cfg) && address != splitdns.BrowserGatewayIP {
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
func darwinAliasExists(ctx context.Context, address string) (bool, error) {
	output, err := darwinRun(ctx, "", "/sbin/ifconfig", "lo0")
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "inet" && fields[1] == address {
			return true, nil
		}
	}
	return false, nil
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
	exists, err := darwinAliasExists(ctx, address)
	if err != nil {
		return err
	}
	if exists && !darwinPlatform.aliases[address] {
		return errors.New("protected address is configured by another owner")
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
		return err
	}
	if err := darwinValidatePolicy(ctx); err != nil {
		return err
	}
	rules, err := darwinRules(ctx, leases, protectedLoopbackCIDRs(cfg)...)
	if err != nil {
		return err
	}
	if _, err = darwinRun(ctx, rules, "/sbin/pfctl", "-a", darwinAnchor, "-f", "-"); err != nil {
		return err
	}
	status, err := darwinRun(ctx, "", "/sbin/pfctl", "-s", "info")
	if err != nil {
		return err
	}
	if !strings.Contains(string(status), "Status: Enabled") {
		output, err := darwinRun(ctx, "", "/sbin/pfctl", "-E")
		if err != nil {
			return err
		}
		token := ""
		for _, line := range strings.Split(string(output), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "Token :") {
				token = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "Token :"))
			}
		}
		if token == "" {
			return errors.New("PF enable reference was not returned")
		}
		if err = darwinWrite(filepath.Join(cfg.StateDir, "pf-enable-token"), []byte(token), 0600); err != nil {
			return err
		}
	}
	active := map[string]bool{}
	for _, lease := range leases {
		active[lease.ip] = true
	}
	for address := range darwinPlatform.aliases {
		if active[address] {
			continue
		}
		// Every port on this address is retired. Kill only its states; never flush
		// other Paperboat machines, normal loopback traffic, or system PF policy.
		if _, err = darwinRun(ctx, "", "/sbin/pfctl", "-k", "0.0.0.0/0", "-k", address); err != nil {
			return err
		}
		exists, err := darwinAliasExists(ctx, address)
		if err != nil {
			return err
		}
		if exists {
			if _, err = darwinRun(ctx, "", "/sbin/ifconfig", "lo0", "-alias", address); err != nil {
				return err
			}
		}
		delete(darwinPlatform.aliases, address)
	}
	return darwinSaveAliases()
}

const darwinResolverMarker = "# Managed by Paperboat machine guard\n"

var darwinResolverDirectory = "/etc/resolver"

// cleanupHistoricalLocalNames removes only marker-owned resolver files and
// Paperboat's marked hosts block. New local DNS/resolver projections are gone.
func cleanupHistoricalLocalNames(ctx context.Context, cfg Config) error {
	if err := clearMachineHosts(ctx, cfg); err != nil {
		return err
	}
	entries, err := os.ReadDir(darwinResolverDirectory)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(darwinResolverDirectory, entry.Name())
		info, e := os.Lstat(path)
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() {
			continue
		}
		data, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		if strings.HasPrefix(string(data), darwinResolverMarker) {
			if e = os.Remove(path); e != nil {
				return e
			}
		}
	}
	flushHostsCache()
	return ctx.Err()
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
		exists, err := darwinAliasExists(ctx, address)
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

// PF evaluates quick matches before later anchors. Support the standard Apple
// root policy only, and require our child to precede any other Apple anchor.
// A custom administrator policy is never silently rewritten to make room.
func darwinValidatePolicy(ctx context.Context) error {
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
		return err
	}
	interfaces, err := query("-v", "-s", "Interfaces", "-i", "lo0")
	if err != nil {
		return err
	}
	anchors, err := query("-a", "com.apple", "-s", "Anchors")
	if err != nil {
		return err
	}
	budget := 64
	return validateDarwinPolicy(root, interfaces, anchors, darwinAnchor, func(anchor string) error { return darwinEarlierAnchorEmpty(query, anchor, &budget, 0) })
}
func validateDarwinPolicy(root, interfaces, anchors, ownedAnchor string, inspectEarlier ...func(string) error) error {
	found := false
	for _, line := range strings.Split(root, "\n") {
		line = strings.Join(strings.Fields(line), " ")
		switch line {
		case "":
		case `scrub-anchor "com.apple/*" all`, `scrub-anchor "com.apple/*" all fragment reassemble`:
		case `anchor "com.apple/*" all`:
			if found {
				return errors.New("duplicate Apple PF anchor is unsupported")
			}
			found = true
		default:
			return errors.New("custom PF root policy must be reconciled before protected machine names can start")
		}
	}
	if !found {
		return errors.New("PF root policy must retain the standard Apple wildcard anchor")
	}
	// pfctl -v prints '(skip)' when PFI_IFLAG_SKIP is active; no suffix means
	// filtering is enabled. Reject unknown output rather than infer protection.
	if strings.TrimSpace(interfaces) != "lo0" {
		return errors.New("PF must filter lo0 before protected machine names can start")
	}
	if !strings.HasPrefix(ownedAnchor, "com.apple/") {
		return errors.New("machine guard PF anchor must be an Apple child")
	}
	own := strings.TrimPrefix(ownedAnchor, "com.apple/")
	for _, line := range strings.Split(anchors, "\n") {
		name := strings.TrimPrefix(strings.TrimSpace(line), "com.apple/")
		if name == "" || name == own {
			continue
		}
		if !darwinAnchorName(name) {
			return errors.New("unknown PF child anchor name")
		}
		if name < own {
			if len(inspectEarlier) != 1 {
				return errors.New("an earlier PF child anchor could bypass protected machine names")
			}
			if err := inspectEarlier[0]("com.apple/" + name); err != nil {
				return err
			}
		}
	}
	return nil
}

// PF retains empty named nodes after flushing a task-owned anchor. Only a fully
// inspected empty subtree can precede the guard; live/unknown state fails closed.
func darwinEarlierAnchorEmpty(query func(...string) (string, error), anchor string, budget *int, depth int) error {
	if *budget <= 0 || depth > 16 {
		return errors.New("PF anchor inspection limit exceeded")
	}
	*budget--
	rules, err := query("-a", anchor, "-sr")
	if err != nil {
		return err
	}
	if strings.TrimSpace(rules) != "" {
		return errors.New("an earlier PF child anchor contains rules that could bypass machine protection")
	}
	children, err := query("-a", anchor, "-s", "Anchors")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(children, "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		name = strings.TrimPrefix(name, anchor+"/")
		if !darwinAnchorName(name) {
			return errors.New("unknown nested PF anchor name")
		}
		if err = darwinEarlierAnchorEmpty(query, anchor+"/"+name, budget, depth+1); err != nil {
			return err
		}
	}
	return nil
}
func darwinAnchorName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}
