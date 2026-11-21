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
	for _, tool := range []string{"iptables", "ip6tables"} { // a host can filter only IPv6
		if out, err := run(ctx, tool, "-S", "INPUT"); err == nil && inputFiltered(out) {
			return "iptables"
		}
	}
	if out, err := run(ctx, "nft", "list", "ruleset"); err == nil {
		if len(nftInputChains(out)) > 0 {
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

type nftChainRef struct{ family, table, chain string }

// nftInputChains finds every filter base chain that hooks input with policy drop in the ip, ip6 and inet families
// (arp and bridge chains do not take tcp/udp rules): a host can filter IPv4 and IPv6 in separate tables, and a
// rule in only one of them leaves the other family closed.
func nftInputChains(ruleset string) []nftChainRef {
	var chains []nftChainRef
	var family, table, chain string
	for _, line := range strings.Split(ruleset, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		switch {
		case len(fields) >= 3 && fields[0] == "table":
			family, table, chain = fields[1], fields[2], ""
		case len(fields) >= 2 && fields[0] == "chain":
			chain = fields[1]
		case chain != "" && slices.Contains([]string{"ip", "ip6", "inet"}, family) && strings.HasPrefix(strings.TrimSpace(line), "type filter hook input ") && strings.Contains(line, "policy drop"):
			chains = append(chains, nftChainRef{family, table, chain})
		}
	}
	return chains
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
			if entry := ufwOwned(spec, out); entry != "" {
				added = append(added, entry)
			}
		}
	case "firewalld":
		zones, err := firewalldZones(ctx, run)
		if err != nil {
			return added, err
		}
		for _, zone := range zones {
			for _, r := range rules {
				spec := strings.Replace(r.Ports, ":", "-", 1) + "/" + r.Proto
				if out, err := run(ctx, "firewall-cmd", "--permanent", "--zone="+zone, "--query-port="+spec); err == nil && strings.TrimSpace(out) == "yes" {
					continue
				}
				if _, err := run(ctx, "firewall-cmd", "--permanent", "--zone="+zone, "--add-port="+spec); err != nil {
					return added, err
				}
				added = append(added, zone+" "+spec)
			}
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
		chains := nftInputChains(out)
		if len(chains) == 0 {
			return added, errors.New("no nftables input chain with policy drop")
		}
		for _, c := range chains {
			listed, err := run(ctx, "nft", "list", "chain", c.family, c.table, c.chain)
			if err != nil {
				return added, err
			}
			for _, proto := range []string{"tcp", "udp"} {
				if nftRulePresent(listed, rules, proto) {
					continue
				}
				args := append([]string{"insert", "rule", c.family, c.table, c.chain}, nftSpec(proto, portsOf(rules, proto))...)
				if _, err := run(ctx, "nft", args...); err != nil {
					return added, err
				}
				if len(added) == 0 {
					added = append(added, "nft")
				}
			}
		}
	}
	return added, nil
}

// nftRulePresent reports whether the chain listing holds the paperboat rule for proto.
func nftRulePresent(listed string, rules []firewallRule, proto string) bool {
	want := strings.Join(nftSpec(proto, portsOf(rules, proto)), " ")
	for _, line := range strings.Split(listed, "\n") {
		line, _, _ = strings.Cut(line, " # handle ") // a rule with more conditions is not ours
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}

// noIPv6Filtering recognizes ip6tables failing because the kernel has no IPv6 filter table.
func noIPv6Filtering(output string) bool {
	return strings.Contains(output, "Table does not exist") || strings.Contains(output, "Address family not supported")
}

// splitIptablesLine splits an `iptables -S` line into its arguments (false when a quote is left open), undoing the double quotes (and backslash
// escapes inside them) iptables puts around values with spaces, so a delete replays the exact arguments.
func splitIptablesLine(line string) ([]string, bool) {
	var args []string
	var current strings.Builder
	inQuote, started := false, false
	for i := 0; i < len(line); i++ {
		switch c := line[i]; {
		case inQuote && c == '\\' && i+1 < len(line):
			i++
			current.WriteByte(line[i])
		case c == '"':
			inQuote, started = !inQuote, true
		case !inQuote && (c == ' ' || c == '\t'):
			if started {
				args = append(args, current.String())
				current.Reset()
				started = false
			}
		default:
			current.WriteByte(c)
			started = true
		}
	}
	if started {
		args = append(args, current.String())
	}
	return args, !inQuote
}

// firewalldZones names the zones public traffic enters: the zone of each interface with a default route, or the
// default zone for an interface bound to none. A rule added to another zone would leave the uplink closed.
func firewalldZones(ctx context.Context, run commander) ([]string, error) {
	out, err := run(ctx, "firewall-cmd", "--get-default-zone")
	if err != nil {
		return nil, err
	}
	def := strings.TrimSpace(out)
	var zones []string
	add := func(zone string) {
		if zone != "" && !slices.Contains(zones, zone) {
			zones = append(zones, zone)
		}
	}
	for _, iface := range publicInterfaces(ctx, run) {
		zone, err := run(ctx, "firewall-cmd", "--get-zone-of-interface="+iface)
		if zone = strings.TrimSpace(zone); err != nil || zone == "" || zone == "no zone" {
			zone = def
		}
		add(zone)
	}
	if len(zones) == 0 { // no uplink found: the default zone is where an unbound interface lands
		add(def)
	}
	return zones, nil
}

// ufwRule reads one row of `ufw status` (or `ufw status numbered` after its [N] prefix): the port/protocol spec, whether
// it is the IPv6 row, and whether it is a plain inbound ALLOW from anywhere (not DENY, OUT, FWD or limited to a source).
func ufwRule(line string) (spec string, v6, allowAnywhere bool) {
	line, _, _ = strings.Cut(line, "#")
	fields := strings.Fields(line)
	if len(fields) < 3 {
		return "", false, false
	}
	spec, rest := fields[0], fields[1:]
	if rest[0] == "(v6)" {
		v6, rest = true, rest[1:]
	}
	if len(rest) < 2 || rest[0] != "ALLOW" {
		return spec, v6, false
	}
	rest = rest[1:]
	if rest[0] == "IN" {
		rest = rest[1:]
	}
	source := strings.Join(rest, " ")
	return spec, v6, (!v6 && source == "Anywhere") || (v6 && source == "Anywhere (v6)")
}

// ufwAllows reports whether `ufw status` lists an inbound ALLOW from anywhere for spec, for IPv4 or IPv6.
func ufwAllows(status, spec string, v6 bool) bool {
	for _, line := range strings.Split(status, "\n") {
		if s, isV6, ok := ufwRule(line); ok && s == spec && isV6 == v6 {
			return true
		}
	}
	return false
}

// ufwOwned says what pbh added for `ufw allow spec`: ufw answers per address family ("Rule added", "Rule added
// (v6)", "Skipping adding existing rule [(v6)]"). Both families new (or IPv6 off) is the plain spec; only one new
// family is "spec v4" or "spec v6", so uninstall removes just that row and not the administrator's other one.
func ufwOwned(spec, output string) string {
	var v4, v6 bool
	for _, line := range strings.Split(output, "\n") {
		switch strings.TrimSpace(line) {
		case "Rule added":
			v4 = true
		case "Rule added (v6)":
			v6 = true
		}
	}
	switch {
	case v4 && (v6 || !strings.Contains(output, "(v6)")):
		return spec
	case v4:
		return spec + " v4"
	case v6:
		return spec + " v6"
	}
	return ""
}

// ufwRemove undoes one ufwOwned entry. `ufw delete allow` removes both families, so a one-family entry deletes its
// row by number, looked up again after every delete because the numbers shift.
func ufwRemove(ctx context.Context, run commander, entry string) error {
	spec, family, single := strings.Cut(entry, " ")
	if !single {
		_, err := run(ctx, "ufw", "delete", "allow", spec)
		return err
	}
	for range 4 { // a spec has at most a few rows; the bound stops a listing that never changes
		out, err := run(ctx, "ufw", "status", "numbered")
		if err != nil {
			return err
		}
		number := ""
		for _, line := range strings.Split(out, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "[") {
				continue
			}
			n, row, ok := strings.Cut(strings.TrimPrefix(line, "["), "]")
			if s, isV6, allow := ufwRule(row); ok && allow && s == spec && isV6 == (family == "v6") {
				number = strings.TrimSpace(n)
				break
			}
		}
		if number == "" {
			return nil
		}
		if _, err := run(ctx, "ufw", "--force", "delete", number); err != nil {
			return err
		}
	}
	return nil
}

// ownedIptablesRule reports whether an `iptables -S` rule is one pbh added: its --comment argument is exactly ours.
func ownedIptablesRule(fields []string) bool {
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] == "--comment" && fields[i+1] == firewallComment {
			return true
		}
	}
	return false
}

type nftRuleRef struct{ family, table, chain, handle string }

// ownedNftRules finds the rules pbh added in every table and chain of an `nft -a list ruleset`: a tcp/udp dport
// rule whose comment is exactly ours.
func ownedNftRules(listing string) []nftRuleRef {
	var refs []nftRuleRef
	var family, table, chain string
	for _, line := range strings.Split(listing, "\n") {
		line = strings.TrimSpace(line)
		fields := strings.Fields(line)
		switch {
		case len(fields) >= 3 && fields[0] == "table":
			family, table, chain = fields[1], fields[2], ""
		case len(fields) >= 2 && fields[0] == "chain":
			chain = fields[1]
		case chain != "" && (strings.HasPrefix(line, "tcp dport ") || strings.HasPrefix(line, "udp dport ")) && strings.Contains(line, ` comment "`+firewallComment+`" # handle `):
			handle := strings.TrimSpace(line[strings.Index(line, "# handle ")+len("# handle "):])
			refs = append(refs, nftRuleRef{family, table, chain, handle})
		}
	}
	return refs
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
		for _, entry := range added {
			if err := ufwRemove(ctx, run, entry); err != nil {
				errs = append(errs, err)
			}
		}
	case "firewalld":
		for _, entry := range added { // "<zone> <port/proto>", as firewallAdd recorded it
			zone, spec, _ := strings.Cut(entry, " ")
			if _, err := run(ctx, "firewall-cmd", "--permanent", "--zone="+zone, "--remove-port="+spec); err != nil {
				errs = append(errs, err)
			}
		}
		if len(added) > 0 {
			if _, err := run(ctx, "firewall-cmd", "--reload"); err != nil {
				errs = append(errs, err)
			}
		}
	case "iptables":
		// Found by comment, not by the record, so a rule the service re-added or a lost record cannot leave one behind.
		for _, tool := range []string{"iptables", "ip6tables"} {
			listed, err := run(ctx, tool, "-S", "INPUT")
			if err != nil {
				// A missing tool, or a kernel without IPv6 filtering, has nothing of ours; any other failure must
				// not look like success.
				if !errors.Is(err, exec.ErrNotFound) && !(tool == "ip6tables" && noIPv6Filtering(listed+err.Error())) {
					errs = append(errs, fmt.Errorf("list %s INPUT rules: %w", tool, err))
				}
				continue
			}
			for _, line := range strings.Split(listed, "\n") {
				fields, ok := splitIptablesLine(line)
				if !ok {
					errs = append(errs, fmt.Errorf("%s -S INPUT: unbalanced quote in rule %q", tool, line))
					continue
				}
				if len(fields) < 3 || fields[0] != "-A" || fields[1] != "INPUT" || !ownedIptablesRule(fields) {
					continue
				}
				if _, err := run(ctx, tool, append([]string{"-D", "INPUT"}, fields[2:]...)...); err != nil {
					errs = append(errs, err)
				}
			}
		}
	case "nft":
		listed, err := run(ctx, "nft", "-a", "list", "ruleset")
		if err != nil {
			if !errors.Is(err, exec.ErrNotFound) {
				errs = append(errs, fmt.Errorf("list nftables rules: %w", err))
			}
			break
		}
		for _, r := range ownedNftRules(listed) {
			if _, err := run(ctx, "nft", "delete", "rule", r.family, r.table, r.chain, "handle", r.handle); err != nil {
				errs = append(errs, err)
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
	// iptables and nft rules are found by their comment whether or not the record lists them; ufw and
	// firewalld rules carry no comment, so those are removed from the record.
	kinds := []string{record.Firewall}
	if record.Firewall == "" {
		kinds = []string{"iptables", "nft"}
	}
	for _, kind := range kinds {
		if (kind == "ufw" || kind == "firewalld") && len(record.Added) == 0 {
			continue
		}
		if err := firewallRemove(ctx, run, kind, record.Added); err != nil {
			errs = append(errs, fmt.Errorf("remove %s rules: %w", kind, err))
		} else if record.Firewall != "" {
			fmt.Fprintf(out, "Removed the %s host firewall rules pbh added.\n", kind)
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
	for _, path := range []string{paths.sysctl, paths.udp} { // pbh's own files, even when the record is gone
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
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
		if err != nil {
			return false
		}
		v6 := strings.Contains(out, "(v6)") // ufw adds the IPv6 rule itself when IPv6 is enabled
		for _, r := range rules {
			spec := r.Ports + "/" + r.Proto
			if !ufwAllows(out, spec, false) || (v6 && !ufwAllows(out, spec, true)) {
				return false
			}
		}
		return true
	case "firewalld":
		zones, err := firewalldZones(ctx, run)
		if err != nil {
			return false
		}
		for _, zone := range zones { // what is live, not only what is configured
			for _, r := range rules {
				spec := strings.Replace(r.Ports, ":", "-", 1) + "/" + r.Proto
				if out, err := run(ctx, "firewall-cmd", "--zone="+zone, "--query-port="+spec); err != nil || strings.TrimSpace(out) != "yes" {
					return false
				}
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
		chains := nftInputChains(out)
		if err != nil || len(chains) == 0 {
			return false
		}
		for _, c := range chains {
			listed, err := run(ctx, "nft", "list", "chain", c.family, c.table, c.chain)
			if err != nil || !nftRulePresent(listed, rules, "tcp") || !nftRulePresent(listed, rules, "udp") {
				return false
			}
		}
		return true
	}
	return false
}
