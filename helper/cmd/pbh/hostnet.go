package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/edgeports"
)

// An edge serving public TCP tunnels needs three things from its host: an outgoing (ephemeral) port
// range clear of the public range, a host firewall that lets 443 and the public range in, and (which
// pbh cannot change) a cloud firewall that does the same. pbh sets up the first two on install and
// serve, removes exactly what it added on uninstall, and names the third.

const (
	sysctlPath        = "/etc/sysctl.d/90-paperboat-edge.conf"
	outgoingProcPath  = "/proc/sys/net/ipv4/ip_local_port_range"
	hostnetRecordFile = "hostnet.json"
	firewallComment   = "paperboat-edge"
)

// hostPaths are the files pbh reads and writes; tests point them at a temp directory.
type hostPaths struct{ sysctl, proc, record string }

func defaultHostPaths(stateDir string) hostPaths {
	return hostPaths{sysctl: sysctlPath, proc: outgoingProcPath, record: filepath.Join(stateDir, hostnetRecordFile)}
}

// commander runs one host command and returns its combined output.
type commander func(ctx context.Context, name string, args ...string) (string, error)

func execCommander(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

// hostnetRecord is what pbh changed, kept so uninstall removes exactly that.
type hostnetRecord struct {
	Firewall         string   `json:"firewall,omitempty"`
	Added            []string `json:"added,omitempty"`
	SysctlWritten    bool     `json:"sysctl_written,omitempty"`
	PreviousOutgoing string   `json:"previous_outgoing,omitempty"`
}

func readHostnetRecord(path string) (hostnetRecord, error) {
	var r hostnetRecord
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err == nil {
		err = json.Unmarshal(raw, &r)
	}
	return r, err
}

func writeHostnetRecord(path string, r hostnetRecord) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

var (
	publicRange   = fmt.Sprintf("%d:%d", edgeports.PublicTCPMin, edgeports.PublicTCPMax)
	outgoingRange = fmt.Sprintf("%d %d", edgeports.OutgoingMin, edgeports.OutgoingMax)
)

// cloudRule is the rule an operator adds to the cloud firewall, security group or security list.
func cloudRule() string {
	return fmt.Sprintf("allow inbound TCP 443 and TCP %d-%d from 0.0.0.0/0 (and ::/0 for IPv6)", edgeports.PublicTCPMin, edgeports.PublicTCPMax)
}

// setupHostNetwork moves the outgoing port range clear of the public range and opens the host
// firewall, recording each change. It never touches the cloud firewall.
func setupHostNetwork(ctx context.Context, run commander, paths hostPaths, out io.Writer) error {
	if err := edgeports.Check(); err != nil {
		return commandError("the public TCP port range overlaps the host's outgoing port range; this pbh build is misconfigured")
	}
	record, err := readHostnetRecord(paths.record)
	if err != nil {
		return fmt.Errorf("%w: %w", commandError("read pbh host network record failed"), err)
	}
	if err := setOutgoingRange(ctx, run, paths, &record, out); err != nil {
		_ = writeHostnetRecord(paths.record, record)
		return err
	}
	if record.Firewall == "" {
		record.Firewall = detectFirewall(ctx, run)
	}
	if record.Firewall != "" {
		added, err := firewallAdd(ctx, run, record.Firewall)
		record.Added = mergeStrings(record.Added, added)
		if err != nil {
			_ = writeHostnetRecord(paths.record, record)
			return fmt.Errorf("%w: %w", commandError(fmt.Sprintf("opening the %s host firewall for TCP 443 and %d-%d failed; add the rule by hand or rerun as root", record.Firewall, edgeports.PublicTCPMin, edgeports.PublicTCPMax)), err)
		}
		fmt.Fprintf(out, "Host firewall (%s): TCP 443 and %d-%d allowed.\n", record.Firewall, edgeports.PublicTCPMin, edgeports.PublicTCPMax)
	} else {
		fmt.Fprintln(out, "No active host firewall found; nothing to open on this host.")
	}
	fmt.Fprintf(out, "If this host sits behind a cloud firewall (security group, security list, VPC firewall), %s. pbh cannot change it from the host; run pbh doctor to check.\n", cloudRule())
	return writeHostnetRecord(paths.record, record)
}

func mergeStrings(a, b []string) []string {
	for _, s := range b {
		found := false
		for _, have := range a {
			found = found || have == s
		}
		if !found {
			a = append(a, s)
		}
	}
	return a
}

// setOutgoingRange makes net.ipv4.ip_local_port_range the range above the public one, in the running
// kernel and in a sysctl.d file so it survives a reboot. A later sysctl file that overrides it is named.
func setOutgoingRange(ctx context.Context, run commander, paths hostPaths, record *hostnetRecord, out io.Writer) error {
	current, err := currentOutgoing(paths.proc)
	if err != nil {
		return fmt.Errorf("%w: %w", commandError("could not read the host's outgoing port range"), err)
	}
	want := "net.ipv4.ip_local_port_range = " + outgoingRange + "\n"
	have, _ := os.ReadFile(paths.sysctl)
	if current == outgoingRange && string(have) == want {
		return nil
	}
	if record.PreviousOutgoing == "" && current != outgoingRange {
		record.PreviousOutgoing = current
	}
	if err := os.WriteFile(paths.sysctl, []byte(want), 0o644); err != nil {
		return fmt.Errorf("%w: %w", commandError("could not write "+paths.sysctl+"; run pbh install as root"), err)
	}
	record.SysctlWritten = true
	if _, err := run(ctx, "sysctl", "-w", "net.ipv4.ip_local_port_range="+outgoingRange); err != nil {
		return fmt.Errorf("%w: %w", commandError("could not apply the outgoing port range with sysctl"), err)
	}
	if after, err := currentOutgoing(paths.proc); err != nil || after != outgoingRange {
		return commandError(fmt.Sprintf("the outgoing port range is %q after pbh set %q; another sysctl setting overrides it (check /etc/sysctl.conf and /etc/sysctl.d), and public TCP tunnel ports %d-%d would collide with outgoing connections", after, outgoingRange, edgeports.PublicTCPMin, edgeports.PublicTCPMax))
	}
	fmt.Fprintf(out, "Outgoing port range moved from %s to %s so it stays clear of public TCP tunnel ports.\n", current, outgoingRange)
	return nil
}

func currentOutgoing(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(raw))
	if len(fields) != 2 {
		return "", errors.New("unexpected outgoing port range format")
	}
	return fields[0] + " " + fields[1], nil
}

// detectFirewall names the host firewall that is filtering inbound traffic, or "".
func detectFirewall(ctx context.Context, run commander) string {
	if out, err := run(ctx, "ufw", "status"); err == nil && strings.Contains(out, "Status: active") {
		return "ufw"
	}
	if out, err := run(ctx, "firewall-cmd", "--state"); err == nil && strings.TrimSpace(out) == "running" {
		return "firewalld"
	}
	if out, err := run(ctx, "iptables", "-S", "INPUT"); err == nil && inputFiltered(out) {
		return "iptables"
	}
	if out, err := run(ctx, "nft", "list", "ruleset"); err == nil {
		if _, _, _, ok := nftInputChain(out); ok {
			return "nft"
		}
	}
	return ""
}

// inputFiltered reports whether an `iptables -S INPUT` listing drops what it does not allow.
func inputFiltered(listing string) bool {
	for _, line := range strings.Split(listing, "\n") {
		line = strings.TrimSpace(line)
		if line == "-P INPUT DROP" || line == "-P INPUT REJECT" || strings.HasPrefix(line, "-A INPUT -j REJECT") || strings.HasPrefix(line, "-A INPUT -j DROP") {
			return true
		}
	}
	return false
}

// nftInputChain finds the first base chain that hooks input with policy drop.
func nftInputChain(ruleset string) (family, table, chain string, ok bool) {
	var currentFamily, currentTable, currentChain string
	for _, line := range strings.Split(ruleset, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		switch {
		case len(fields) >= 3 && fields[0] == "table":
			currentFamily, currentTable = fields[1], fields[2]
		case len(fields) >= 2 && fields[0] == "chain":
			currentChain = fields[1]
		case strings.Contains(line, "hook input") && strings.Contains(line, "policy drop") && currentTable != "":
			return currentFamily, currentTable, currentChain, true
		}
	}
	return "", "", "", false
}

func iptablesSpec() []string {
	return []string{"-p", "tcp", "-m", "multiport", "--dports", "443," + publicRange, "-m", "comment", "--comment", firewallComment, "-j", "ACCEPT"}
}

// firewallAdd opens 443 and the public range and returns what it added (rules that were already
// there are not returned, so uninstall leaves them alone).
func firewallAdd(ctx context.Context, run commander, kind string) ([]string, error) {
	var added []string
	switch kind {
	case "ufw":
		for _, spec := range []string{"443/tcp", publicRange + "/tcp"} {
			out, err := run(ctx, "ufw", "allow", spec)
			if err != nil {
				return added, err
			}
			if !strings.Contains(out, "existing") {
				added = append(added, spec)
			}
		}
	case "firewalld":
		for _, spec := range []string{"443/tcp", strings.Replace(publicRange, ":", "-", 1) + "/tcp"} {
			if out, err := run(ctx, "firewall-cmd", "--query-port="+spec); err == nil && strings.TrimSpace(out) == "yes" {
				continue
			}
			if _, err := run(ctx, "firewall-cmd", "--permanent", "--add-port="+spec); err != nil {
				return added, err
			}
			added = append(added, spec)
		}
		if len(added) > 0 {
			if _, err := run(ctx, "firewall-cmd", "--reload"); err != nil {
				return added, err
			}
		}
	case "iptables":
		for _, tool := range iptablesTools(ctx, run) {
			if _, err := run(ctx, tool, append([]string{"-C", "INPUT"}, iptablesSpec()...)...); err == nil {
				continue
			}
			if _, err := run(ctx, tool, append([]string{"-I", "INPUT", "1"}, iptablesSpec()...)...); err != nil {
				return added, err
			}
			added = append(added, tool)
		}
	case "nft":
		out, err := run(ctx, "nft", "list", "ruleset")
		if err != nil {
			return added, err
		}
		family, table, chain, ok := nftInputChain(out)
		if !ok {
			return added, errors.New("no nftables input chain with policy drop")
		}
		listed, err := run(ctx, "nft", "list", "chain", family, table, chain)
		if err == nil && strings.Contains(listed, firewallComment) {
			return added, nil
		}
		if _, err := run(ctx, "nft", "insert", "rule", family, table, chain, "tcp", "dport", "{", "443,", strings.Replace(publicRange, ":", "-", 1), "}", "accept", "comment", `"`+firewallComment+`"`); err != nil {
			return added, err
		}
		added = append(added, "nft")
	}
	return added, nil
}

// iptablesTools lists iptables, plus ip6tables when IPv6 input is filtered too.
func iptablesTools(ctx context.Context, run commander) []string {
	tools := []string{"iptables"}
	if out, err := run(ctx, "ip6tables", "-S", "INPUT"); err == nil && inputFiltered(out) {
		tools = append(tools, "ip6tables")
	}
	return tools
}

// firewallRemove undoes firewallAdd for the rules recorded.
func firewallRemove(ctx context.Context, run commander, kind string, added []string) error {
	var errs []error
	switch kind {
	case "ufw":
		for _, spec := range added {
			if _, err := run(ctx, "ufw", "delete", "allow", spec); err != nil {
				errs = append(errs, err)
			}
		}
	case "firewalld":
		for _, spec := range added {
			if _, err := run(ctx, "firewall-cmd", "--permanent", "--remove-port="+spec); err != nil {
				errs = append(errs, err)
			}
		}
		if len(added) > 0 {
			if _, err := run(ctx, "firewall-cmd", "--reload"); err != nil {
				errs = append(errs, err)
			}
		}
	case "iptables":
		for _, tool := range added {
			if _, err := run(ctx, tool, append([]string{"-D", "INPUT"}, iptablesSpec()...)...); err != nil {
				errs = append(errs, err)
			}
		}
	case "nft":
		if len(added) == 0 {
			break
		}
		out, err := run(ctx, "nft", "list", "ruleset")
		family, table, chain, ok := nftInputChain(out)
		if err != nil || !ok {
			errs = append(errs, errors.New("nftables input chain not found"))
			break
		}
		listed, err := run(ctx, "nft", "-a", "list", "chain", family, table, chain)
		if err != nil {
			errs = append(errs, err)
			break
		}
		for _, line := range strings.Split(listed, "\n") {
			if i := strings.Index(line, "# handle "); i >= 0 && strings.Contains(line, firewallComment) {
				if _, err := run(ctx, "nft", "delete", "rule", family, table, chain, "handle", strings.TrimSpace(line[i+len("# handle "):])); err != nil {
					errs = append(errs, err)
				}
			}
		}
	}
	return errors.Join(errs...)
}

// reapplyHostFirewall re-adds a runtime-only rule (iptables, nft) after a reboot; the service calls it
// on every start. ufw and firewalld rules persist on their own.
func reapplyHostFirewall(ctx context.Context, run commander, paths hostPaths, out io.Writer) {
	record, err := readHostnetRecord(paths.record)
	if err != nil || (record.Firewall != "iptables" && record.Firewall != "nft") {
		return
	}
	if _, err := firewallAdd(ctx, run, record.Firewall); err != nil {
		fmt.Fprintf(out, "pbh: could not re-open the %s host firewall for the public TCP range: %v\n", record.Firewall, err)
	}
}

// removeHostNetwork undoes setupHostNetwork: the firewall rules pbh added, its sysctl file, and the
// outgoing range it replaced.
func removeHostNetwork(ctx context.Context, run commander, paths hostPaths, out io.Writer) error {
	record, err := readHostnetRecord(paths.record)
	if err != nil {
		return fmt.Errorf("%w: %w", commandError("read pbh host network record failed; nothing removed"), err)
	}
	var errs []error
	if record.Firewall != "" && len(record.Added) > 0 {
		if err := firewallRemove(ctx, run, record.Firewall, record.Added); err != nil {
			errs = append(errs, fmt.Errorf("remove %s rules: %w", record.Firewall, err))
		} else {
			fmt.Fprintf(out, "Removed the %s host firewall rules pbh added.\n", record.Firewall)
		}
	}
	if record.SysctlWritten {
		if err := os.Remove(paths.sysctl); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		} else if record.PreviousOutgoing != "" {
			if _, err := run(ctx, "sysctl", "-w", "net.ipv4.ip_local_port_range="+record.PreviousOutgoing); err != nil {
				errs = append(errs, fmt.Errorf("restore outgoing port range %s: %w", record.PreviousOutgoing, err))
			} else {
				fmt.Fprintf(out, "Outgoing port range restored to %s.\n", record.PreviousOutgoing)
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w: %w", commandError("could not remove every host network change pbh made; the record is kept so uninstall can be rerun"), errors.Join(errs...))
	}
	if err := os.Remove(paths.record); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// firewallRulePresent reports whether the rule firewallAdd installs is active now.
func firewallRulePresent(ctx context.Context, run commander, kind string) bool {
	switch kind {
	case "ufw":
		out, err := run(ctx, "ufw", "status")
		return err == nil && strings.Contains(out, publicRange) && strings.Contains(out, "443")
	case "firewalld":
		for _, spec := range []string{"443/tcp", strings.Replace(publicRange, ":", "-", 1) + "/tcp"} {
			if out, err := run(ctx, "firewall-cmd", "--query-port="+spec); err != nil || strings.TrimSpace(out) != "yes" {
				return false
			}
		}
		return true
	case "iptables":
		for _, tool := range iptablesTools(ctx, run) {
			if _, err := run(ctx, tool, append([]string{"-C", "INPUT"}, iptablesSpec()...)...); err != nil {
				return false
			}
		}
		return true
	case "nft":
		out, err := run(ctx, "nft", "list", "ruleset")
		family, table, chain, ok := nftInputChain(out)
		if err != nil || !ok {
			return false
		}
		listed, err := run(ctx, "nft", "list", "chain", family, table, chain)
		return err == nil && strings.Contains(listed, firewallComment)
	}
	return false
}
