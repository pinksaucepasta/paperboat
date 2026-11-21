package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/relay/selfhost"
	"github.com/pinksaucepasta/paperboat/shared/edgeports"
)

// An edge serving public TCP tunnels needs three things from its host: an outgoing (ephemeral) port
// range clear of the public range, a host firewall that lets 443, the carrier and relay ports and the public range in, and (which
// pbh cannot change) a cloud firewall that does the same. pbh sets up the first two on install and
// serve, removes exactly what it added on uninstall, and names the third. It also applies the
// per-address connection limits (edge_guard.go).

const (
	sysctlPath        = "/etc/sysctl.d/90-paperboat-edge.conf"
	udpSysctlPath     = "/etc/sysctl.d/90-paperboat-edge-udp.conf"
	outgoingProcPath  = "/proc/sys/net/ipv4/ip_local_port_range"
	hostnetRecordFile = "hostnet.json"
	edgeGuardFile     = "edge-guard.nft"
	firewallComment   = "paperboat-edge"
	// udpBufferBytes is the socket buffer quic-go asks for (7 MiB); the kernel caps a socket at net.core.rmem_max/wmem_max.
	udpBufferBytes = 7 << 20
)

// udpBufferKeys are the host limits behind udpBufferBytes.
var udpBufferKeys = []string{"net.core.rmem_max", "net.core.wmem_max"}

// hostPaths are the files pbh reads and writes; tests point them at a temp directory.
type hostPaths struct{ sysctl, udp, proc, record, guard string }

func defaultHostPaths(stateDir string) hostPaths {
	return hostPaths{sysctl: sysctlPath, udp: udpSysctlPath, proc: outgoingProcPath, record: filepath.Join(stateDir, hostnetRecordFile), guard: filepath.Join(stateDir, edgeGuardFile)}
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
	Firewall string   `json:"firewall,omitempty"`
	Added    []string `json:"added,omitempty"`
	// Rules is what the firewall rules were built from, so a runtime-only firewall is re-opened with the same set.
	Rules            []firewallRule `json:"rules,omitempty"`
	SysctlWritten    bool           `json:"sysctl_written,omitempty"`
	PreviousOutgoing string         `json:"previous_outgoing,omitempty"`
	// PreviousUDP maps each UDP buffer limit pbh raised to the value it had before.
	PreviousUDP map[string]string `json:"previous_udp,omitempty"`
	Guard       bool              `json:"guard,omitempty"`
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

// firewallRule is one inbound allow rule: a protocol and a port or a colon range.
type firewallRule struct {
	Proto string `json:"proto"`
	Ports string `json:"ports"`
}

// hostFirewallRules is what a node's host firewall must let in, from the ports its components really
// listen on: the claim port; a relay's WSS (TCP), STUN (UDP, same port) and QUIC; a tunnel edge's 443
// (TCP, and UDP for HTTP/3), carrier TCP and QUIC, and the public tunnel range.
func hostFirewallRules(c selfhost.Config) []firewallRule {
	var rules []firewallRule
	add := func(proto, ports string) {
		if r := (firewallRule{proto, ports}); !slices.Contains(rules, r) {
			rules = append(rules, r)
		}
	}
	add("tcp", strconv.Itoa(claimPortOf(c.Listen)))
	for _, v := range c.Components {
		switch v.Capability {
		case "relay":
			add("tcp", strconv.Itoa(v.TCPPort))
			add("udp", strconv.Itoa(v.TCPPort))
			add("udp", strconv.Itoa(v.QUICPort))
		case "tunnel":
			add("tcp", "443")
			add("udp", "443")
			add("tcp", strconv.Itoa(v.TCPPort))
			add("udp", strconv.Itoa(v.QUICPort))
			add("tcp", publicRange)
		}
	}
	return rules
}

// rulesText names the rules the way an operator adds them to a cloud firewall: "TCP 443, 7272-7275 and UDP 443".
func rulesText(rules []firewallRule) string {
	var parts []string
	for _, proto := range []string{"tcp", "udp"} {
		var ports []string
		for _, r := range rules {
			if r.Proto == proto {
				ports = append(ports, strings.Replace(r.Ports, ":", "-", 1))
			}
		}
		if len(ports) > 0 {
			parts = append(parts, strings.ToUpper(proto)+" "+strings.Join(ports, ", "))
		}
	}
	return strings.Join(parts, " and ")
}

// cloudRule is the rule an operator adds to the cloud firewall, security group or security list.
func cloudRule(rules []firewallRule) string {
	return "allow inbound " + rulesText(rules) + " from 0.0.0.0/0 (and ::/0 for IPv6)"
}

// claimPortOf is the TCP port of the claim listener address (edgeports.Claim by default).
func claimPortOf(listen string) int {
	_, p, err := net.SplitHostPort(listen)
	if n, e := strconv.Atoi(p); err == nil && e == nil {
		return n
	}
	return edgeports.Claim
}

// setupHostNetwork moves the outgoing port range clear of the public range and opens the host
// firewall, recording each change. It never touches the cloud firewall. A node without a tunnel keeps
// the outgoing range and skips the edge guard, which only serve public tunnel ports.
func setupHostNetwork(ctx context.Context, run commander, paths hostPaths, out io.Writer, c selfhost.Config) error {
	if err := edgeports.Check(); err != nil {
		return commandError("the public TCP port range overlaps the host's outgoing port range; this pbh build is misconfigured")
	}
	tunnel := slices.ContainsFunc(c.Components, func(v selfhost.Component) bool { return v.Capability == "tunnel" })
	record, err := readHostnetRecord(paths.record)
	if err != nil {
		return fmt.Errorf("%w: %w", commandError("read pbh host network record failed"), err)
	}
	if tunnel {
		if err := setOutgoingRange(ctx, run, paths, &record, out); err != nil {
			_ = writeHostnetRecord(paths.record, record)
			return err
		}
	}
	if err := setUDPBuffers(ctx, run, paths, &record, out); err != nil {
		_ = writeHostnetRecord(paths.record, record)
		return err
	}
	if record.Firewall == "" {
		record.Firewall = detectFirewall(ctx, run)
	}
	if record.Firewall != "" {
		record.Rules = hostFirewallRules(c)
		added, err := firewallAdd(ctx, run, record.Firewall, record.Rules)
		record.Added = mergeStrings(record.Added, added)
		if err != nil {
			_ = writeHostnetRecord(paths.record, record)
			return fmt.Errorf("%w: %w", commandError(fmt.Sprintf("opening the %s host firewall for %s failed; add the rule by hand or rerun as root", record.Firewall, rulesText(record.Rules))), err)
		}
		fmt.Fprintf(out, "Host firewall (%s): %s allowed.\n", record.Firewall, rulesText(record.Rules))
	} else {
		fmt.Fprintln(out, "No active host firewall found; nothing to open on this host.")
	}
	if tunnel {
		if err := setupEdgeGuard(ctx, run, paths, &record, out); err != nil {
			_ = writeHostnetRecord(paths.record, record)
			return err
		}
	}
	fmt.Fprintf(out, "If this host sits behind a cloud firewall (security group, security list, VPC firewall), %s. pbh cannot change it from the host; run pbh doctor to check.\n", cloudRule(hostFirewallRules(c)))
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

// setUDPBuffers raises net.core.rmem_max and wmem_max to what quic-go asks for, in the running kernel and in a
// sysctl.d file. The edge's QUIC listeners cannot raise them (the container has no NET_ADMIN) and at the stock
// 208 KiB a single stream at 50 ms RTT loses packets and stalls (PR-06). A value already higher is left alone.
// The edge only sees the new limit on sockets it opens afterwards, so the edge is restarted after this runs.
func setUDPBuffers(ctx context.Context, run commander, paths hostPaths, record *hostnetRecord, out io.Writer) error {
	var file string
	for _, key := range udpBufferKeys {
		raw, err := run(ctx, "sysctl", "-n", key)
		have, convErr := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || convErr != nil {
			return fmt.Errorf("%w: %w", commandError("could not read "+key+" with sysctl"), errors.Join(err, convErr))
		}
		_, ours := record.PreviousUDP[key]
		if have >= udpBufferBytes && !ours {
			continue
		}
		if !ours {
			if record.PreviousUDP == nil {
				record.PreviousUDP = map[string]string{}
			}
			record.PreviousUDP[key] = strconv.Itoa(have)
		}
		file += fmt.Sprintf("%s = %d\n", key, udpBufferBytes)
		if have == udpBufferBytes {
			continue
		}
		if _, err := run(ctx, "sysctl", "-w", fmt.Sprintf("%s=%d", key, udpBufferBytes)); err != nil {
			return fmt.Errorf("%w: %w", commandError("could not raise "+key+" with sysctl; QUIC tunnels would be slow at long distances"), err)
		}
		fmt.Fprintf(out, "%s raised from %d to %d for QUIC.\n", key, have, udpBufferBytes)
	}
	if file == "" {
		return nil
	}
	if err := os.WriteFile(paths.udp, []byte(file), 0o644); err != nil {
		return fmt.Errorf("%w: %w", commandError("could not write "+paths.udp+"; run pbh install as root"), err)
	}
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

// portsOf lists the ports of the rules that use proto, in the colon form ufw and iptables take.
func portsOf(rules []firewallRule, proto string) []string {
	var ports []string
	for _, r := range rules {
		if r.Proto == proto {
			ports = append(ports, r.Ports)
		}
	}
	return ports
}

// iptablesSpec is the one rule per protocol that allows its ports (multiport).
func iptablesSpec(proto string, ports []string) []string {
	return []string{"-p", proto, "-m", "multiport", "--dports", strings.Join(ports, ","), "-m", "comment", "--comment", firewallComment, "-j", "ACCEPT"}
}

// nftSpec is the rule that allows the ports of one protocol, after the nft insert/ chain/ table words.
func nftSpec(proto string, ports []string) []string {
	sorted := slices.Clone(ports) // nft lists a set in ascending order; the same order lets the rule be recognized again
	slices.SortFunc(sorted, func(a, b string) int { return portStart(a) - portStart(b) })
	return []string{proto, "dport", "{", strings.Replace(strings.Join(sorted, ", "), ":", "-", -1), "}", "accept", "comment", `"` + firewallComment + `"`}
}

func portStart(ports string) int {
	n, _, _ := strings.Cut(ports, ":")
	v, _ := strconv.Atoi(n)
	return v
}

// firewallAdd opens the rules and returns what it added (rules that were already
// there are not returned, so uninstall leaves them alone).
func firewallAdd(ctx context.Context, run commander, kind string, rules []firewallRule) ([]string, error) {
	var added []string
	switch kind {
	case "ufw":
		for _, r := range rules {
			spec := r.Ports + "/" + r.Proto
			out, err := run(ctx, "ufw", "allow", spec)
			if err != nil {
				return added, err
			}
			if !strings.Contains(out, "existing") {
				added = append(added, spec)
			}
		}
	case "firewalld":
		for _, r := range rules {
			spec := strings.Replace(r.Ports, ":", "-", 1) + "/" + r.Proto
			if out, err := run(ctx, "firewall-cmd", "--permanent", "--query-port="+spec); err == nil && strings.TrimSpace(out) == "yes" {
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
			for _, proto := range []string{"tcp", "udp"} {
				spec := iptablesSpec(proto, portsOf(rules, proto))
				if _, err := run(ctx, tool, append([]string{"-C", "INPUT"}, spec...)...); err == nil {
					continue
				}
				if _, err := run(ctx, tool, append([]string{"-I", "INPUT", "1"}, spec...)...); err != nil {
					return added, err
				}
				added = append(added, tool+" "+strings.Join(spec, " "))
			}
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
		if err != nil {
			return added, err
		}
		for _, proto := range []string{"tcp", "udp"} {
			if nftRulePresent(listed, rules, proto) {
				continue
			}
			args := append([]string{"insert", "rule", family, table, chain}, nftSpec(proto, portsOf(rules, proto))...)
			if _, err := run(ctx, "nft", args...); err != nil {
				return added, err
			}
			if len(added) == 0 {
				added = append(added, "nft")
			}
		}
	}
	return added, nil
}

// nftRulePresent reports whether the chain listing holds the paperboat rule for proto.
func nftRulePresent(listed string, rules []firewallRule, proto string) bool {
	want := strings.Join(nftSpec(proto, portsOf(rules, proto)), " ")
	for _, line := range strings.Split(listed, "\n") {
		if strings.Contains(line, want) {
			return true
		}
	}
	return false
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
		for _, entry := range added { // "<tool> <rule args>", as firewallAdd recorded it
			fields := strings.Fields(entry)
			if len(fields) < 2 {
				continue
			}
			if _, err := run(ctx, fields[0], append([]string{"-D", "INPUT"}, fields[1:]...)...); err != nil {
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

// reapplyHostFirewall re-adds a runtime-only rule (iptables, nft) and the edge guard after a reboot;
// the service calls it on every start. ufw and firewalld rules persist on their own.
func reapplyHostFirewall(ctx context.Context, run commander, paths hostPaths, out io.Writer) {
	record, err := readHostnetRecord(paths.record)
	if err != nil {
		return
	}
	if record.Firewall == "iptables" || record.Firewall == "nft" {
		if len(record.Rules) == 0 {
			fmt.Fprintf(out, "pbh: the host network record lists no firewall rules to re-open; run pbh install again\n")
		} else {
			// Rules the service recreates are pbh's, so uninstall must remove them too.
			added, err := firewallAdd(ctx, run, record.Firewall, record.Rules)
			if len(added) > 0 {
				record.Added = mergeStrings(record.Added, added)
				_ = writeHostnetRecord(paths.record, record)
			}
			if err != nil {
				fmt.Fprintf(out, "pbh: could not re-open the %s host firewall for %s: %v\n", record.Firewall, rulesText(record.Rules), err)
			}
		}
	}
	if record.Guard {
		if skipped, err := applyEdgeGuard(ctx, run, paths.guard); err != nil || skipped != "" {
			fmt.Fprintf(out, "pbh: could not re-apply the per-address connection limits (nftables table %s): %v%s\n", edgeGuardTable, err, skipped)
		}
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
	if record.Guard {
		if err := removeEdgeGuard(ctx, run); err != nil {
			errs = append(errs, fmt.Errorf("remove nftables table %s: %w", edgeGuardTable, err))
		} else {
			_ = os.Remove(paths.guard)
			fmt.Fprintln(out, "Removed the per-address connection limits.")
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
	if len(record.PreviousUDP) > 0 {
		if err := os.Remove(paths.udp); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
		for key, previous := range record.PreviousUDP {
			if _, err := run(ctx, "sysctl", "-w", key+"="+previous); err != nil {
				errs = append(errs, fmt.Errorf("restore %s=%s: %w", key, previous, err))
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

// firewallRulePresent reports whether the rules firewallAdd installs are active now.
func firewallRulePresent(ctx context.Context, run commander, kind string, rules []firewallRule) bool {
	switch kind {
	case "ufw":
		out, err := run(ctx, "ufw", "status")
		for _, r := range rules {
			if err != nil || !strings.Contains(out, r.Ports+"/"+r.Proto) {
				return false
			}
		}
		return true
	case "firewalld":
		for _, r := range rules {
			spec := strings.Replace(r.Ports, ":", "-", 1) + "/" + r.Proto
			if out, err := run(ctx, "firewall-cmd", "--permanent", "--query-port="+spec); err != nil || strings.TrimSpace(out) != "yes" {
				return false
			}
		}
		return true
	case "iptables":
		for _, tool := range iptablesTools(ctx, run) {
			for _, proto := range []string{"tcp", "udp"} {
				if _, err := run(ctx, tool, append([]string{"-C", "INPUT"}, iptablesSpec(proto, portsOf(rules, proto))...)...); err != nil {
					return false
				}
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
		return err == nil && nftRulePresent(listed, rules, "tcp") && nftRulePresent(listed, rules, "udp")
	}
	return false
}
