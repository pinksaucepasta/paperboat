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
		if !validOwnedLoopbackAddress(address) && address != splitdns.BrowserGatewayIP {
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
