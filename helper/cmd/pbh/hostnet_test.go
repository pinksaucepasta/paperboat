package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/relay/selfhost"
)

// fakeHost answers host commands from a table and records every call; a `sysctl -w` rewrites the
// outgoing-range file the way the kernel would.
type fakeHost struct {
	paths     hostPaths
	answers   map[string]string // command line -> output; a missing key is an error
	calls     []string
	sysctlOff bool              // a sysctl that does not change the range (something overrides it)
	udp       map[string]string // net.core.rmem_max / wmem_max as the kernel holds them
}

func newFakeHost(t *testing.T, outgoing string) *fakeHost {
	dir := t.TempDir()
	f := &fakeHost{paths: hostPaths{sysctl: filepath.Join(dir, "sysctl.conf"), proc: filepath.Join(dir, "range"), record: filepath.Join(dir, "hostnet.json"), guard: filepath.Join(dir, "edge-guard.nft"), udp: filepath.Join(dir, "udp.conf")},
		answers: map[string]string{}, udp: map[string]string{"net.core.rmem_max": "212992", "net.core.wmem_max": "212992"}}
	if err := os.WriteFile(f.paths.proc, []byte(outgoing+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fakeHost) run(_ context.Context, name string, args ...string) (string, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, line)
	if name == "sysctl" && args[0] == "-n" {
		return f.udp[args[1]] + "\n", nil
	}
	if name == "sysctl" {
		if key, value, _ := strings.Cut(args[1], "="); strings.HasPrefix(key, "net.core.") {
			f.udp[key] = value
		} else if !f.sysctlOff {
			_ = os.WriteFile(f.paths.proc, []byte(value+"\n"), 0o644)
		}
		return "", nil
	}
	if out, ok := f.answers[line]; ok {
		return out, nil
	}
	if name == "nft" || ((name == "iptables" || name == "ip6tables") && args[0] == "-S") {
		return "", fmt.Errorf("%s: %w", line, exec.ErrNotFound) // a host without nftables or iptables
	}
	return "", errors.New("not found: " + line)
}

func (f *fakeHost) called(prefix string) int {
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func TestSetupMovesTheOutgoingRangeAndUninstallRestoresIt(t *testing.T) {
	f := newFakeHost(t, "32768 60999")
	var out strings.Builder
	if err := setupHostNetwork(t.Context(), f.run, f.paths, &out, testConfig); err != nil {
		t.Fatal(err)
	}
	if got, _ := currentOutgoing(f.paths.proc); got != "60000 65535" {
		t.Fatalf("outgoing range = %q", got)
	}
	if file, _ := os.ReadFile(f.paths.sysctl); string(file) != "net.ipv4.ip_local_port_range = 60000 65535\n" {
		t.Fatalf("sysctl file = %q", file)
	}
	if !strings.Contains(out.String(), "cloud firewall") || !strings.Contains(out.String(), "10000-59999") {
		t.Fatalf("no cloud rule printed: %q", out.String())
	}
	if err := removeHostNetwork(t.Context(), f.run, f.paths, &out); err != nil {
		t.Fatal(err)
	}
	if got, _ := currentOutgoing(f.paths.proc); got != "32768 60999" {
		t.Fatalf("outgoing range after uninstall = %q", got)
	}
	for _, gone := range []string{f.paths.sysctl, f.paths.udp, f.paths.record} {
		if _, err := os.Stat(gone); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s survived uninstall: %v", gone, err)
		}
	}
}

func TestSetupRaisesTheUDPBuffersForQUICAndUninstallRestoresThem(t *testing.T) {
	f := newFakeHost(t, "60000 65535")
	if err := setupHostNetwork(t.Context(), f.run, f.paths, &strings.Builder{}, testConfig); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprint(udpBufferBytes)
	if f.udp["net.core.rmem_max"] != want || f.udp["net.core.wmem_max"] != want {
		t.Fatalf("limits after setup = %v", f.udp)
	}
	if file, _ := os.ReadFile(f.paths.udp); string(file) != "net.core.rmem_max = "+want+"\nnet.core.wmem_max = "+want+"\n" {
		t.Fatalf("udp sysctl file = %q", file)
	}
	// A rerun keeps the original values for uninstall.
	if err := setupHostNetwork(t.Context(), f.run, f.paths, &strings.Builder{}, testConfig); err != nil {
		t.Fatal(err)
	}
	if err := removeHostNetwork(t.Context(), f.run, f.paths, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if f.udp["net.core.rmem_max"] != "212992" || f.udp["net.core.wmem_max"] != "212992" {
		t.Fatalf("limits after uninstall = %v", f.udp)
	}
}

func TestSetupNeverLowersAHigherUDPBuffer(t *testing.T) {
	f := newFakeHost(t, "60000 65535")
	f.udp["net.core.rmem_max"] = "26214400"
	if err := setupHostNetwork(t.Context(), f.run, f.paths, &strings.Builder{}, testConfig); err != nil {
		t.Fatal(err)
	}
	if f.udp["net.core.rmem_max"] != "26214400" || f.udp["net.core.wmem_max"] != fmt.Sprint(udpBufferBytes) {
		t.Fatalf("limits = %v", f.udp)
	}
	if err := removeHostNetwork(t.Context(), f.run, f.paths, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if f.udp["net.core.rmem_max"] != "26214400" {
		t.Fatalf("a limit pbh never changed was touched: %v", f.udp)
	}
}

func TestSetupLeavesACorrectRangeAlone(t *testing.T) {
	f := newFakeHost(t, "60000 65535")
	if err := os.WriteFile(f.paths.sysctl, []byte("net.ipv4.ip_local_port_range = 60000 65535\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := setupHostNetwork(t.Context(), f.run, f.paths, &strings.Builder{}, testConfig); err != nil {
		t.Fatal(err)
	}
	if f.called("sysctl -w net.ipv4") != 0 {
		t.Fatalf("sysctl ran for an already correct range: %v", f.calls)
	}
}

func TestSetupRefusesWhenAnotherSysctlOverridesTheRange(t *testing.T) {
	f := newFakeHost(t, "32768 60999")
	f.sysctlOff = true
	err := setupHostNetwork(t.Context(), f.run, f.paths, &strings.Builder{}, testConfig)
	var command commandError
	if !errors.As(err, &command) || !strings.Contains(err.Error(), "another sysctl setting overrides it") {
		t.Fatalf("err = %v", err)
	}
}

func TestDetectFirewall(t *testing.T) {
	for name, test := range map[string]struct {
		answers map[string]string
		want    string
	}{
		"ufw":                   {map[string]string{"ufw status": "Status: active\n"}, "ufw"},
		"ufw inactive":          {map[string]string{"ufw status": "Status: inactive\n"}, ""},
		"firewalld":             {map[string]string{"firewall-cmd --state": "running\n"}, "firewalld"},
		"iptables drop":         {map[string]string{"iptables -S INPUT": "-P INPUT DROP\n-A INPUT -i lo -j ACCEPT\n"}, "iptables"},
		"iptables final reject": {map[string]string{"iptables -S INPUT": "-P INPUT ACCEPT\n-A INPUT -p tcp -m tcp --dport 22 -j ACCEPT\n-A INPUT -j REJECT --reject-with icmp-host-prohibited\n"}, "iptables"},
		"iptables open":         {map[string]string{"iptables -S INPUT": "-P INPUT ACCEPT\n"}, ""},
		"ip6tables only drop":   {map[string]string{"iptables -S INPUT": "-P INPUT ACCEPT\n", "ip6tables -S INPUT": "-P INPUT DROP\n"}, "iptables"},
		"nft only":              {map[string]string{"nft list ruleset": "table inet filter {\n\tchain input {\n\t\ttype filter hook input priority filter; policy drop;\n\t}\n}\n"}, "nft"},
		"nothing":               {map[string]string{}, ""},
	} {
		f := newFakeHost(t, "60000 65535")
		f.answers = test.answers
		if got := detectFirewall(t.Context(), f.run); got != test.want {
			t.Errorf("%s: detected %q, want %q", name, got, test.want)
		}
	}
}

// testConfig is a tunnel node on the default ports: claim 7277, carrier TCP 7272 and QUIC 7273.
var testConfig = selfhost.Config{Listen: "[::]:7277", Components: []selfhost.Component{{Capability: "tunnel", TCPPort: 7272, QUICPort: 7273}}}

var testRules = hostFirewallRules(testConfig)

// The rules come from the ports the components listen on, TCP and UDP where each uses it.
func TestHostFirewallRulesFollowTheComponents(t *testing.T) {
	relay := func(tcp, quic int) selfhost.Component {
		return selfhost.Component{Capability: "relay", TCPPort: tcp, QUICPort: quic}
	}
	for name, test := range map[string]struct {
		config selfhost.Config
		want   string
	}{
		"tunnel":     {testConfig, "TCP 7277, 443, 7272, 10000-59999 and UDP 443, 7273"},
		"relay only": {selfhost.Config{Listen: "[::]:7277", Components: []selfhost.Component{relay(443, 7275)}}, "TCP 7277, 443 and UDP 443, 7275"},
		"both":       {selfhost.Config{Listen: "[::]:9000", Components: []selfhost.Component{relay(7274, 7275), {Capability: "tunnel", TCPPort: 7272, QUICPort: 7273}}}, "TCP 9000, 7274, 443, 7272, 10000-59999 and UDP 7274, 7275, 443, 7273"},
	} {
		if got := rulesText(hostFirewallRules(test.config)); got != test.want {
			t.Errorf("%s: rules = %q, want %q", name, got, test.want)
		}
	}
	if claimPortOf("[::]:7277") != 7277 || claimPortOf("127.0.0.1:9000") != 9000 || claimPortOf("bad") != 7277 {
		t.Fatal("claim port not read from the claim listen address")
	}
}

func TestFirewallAddAndRemovePerKind(t *testing.T) {
	tcp := strings.Join(iptablesSpec("tcp", portsOf(testRules, "tcp")), " ")
	udp := strings.Join(iptablesSpec("udp", portsOf(testRules, "udp")), " ")
	const nftTCP = `tcp dport { 443, 7272, 7277, 10000-59999 } accept comment "paperboat-edge"`
	const nftUDP = `udp dport { 443, 7273 } accept comment "paperboat-edge"`
	for name, test := range map[string]struct {
		kind    string
		answers map[string]string
		added   []string
		wantAdd []string // command prefixes that must have run on add
		wantDel []string // ... and on remove
	}{
		"ufw": {"ufw", map[string]string{"ufw allow 7277/tcp": "Rule added", "ufw allow 443/tcp": "Rule added", "ufw allow 443/udp": "Rule added", "ufw allow 7272/tcp": "Rule added", "ufw allow 7273/udp": "Rule added", "ufw allow 10000:59999/tcp": "Skipping adding existing rule",
			"ufw delete allow 7277/tcp": "", "ufw delete allow 443/tcp": "", "ufw delete allow 443/udp": "", "ufw delete allow 7272/tcp": "", "ufw delete allow 7273/udp": ""},
			[]string{"7277/tcp", "443/tcp", "443/udp", "7272/tcp", "7273/udp"},
			[]string{"ufw allow 7277/tcp", "ufw allow 443/udp", "ufw allow 7273/udp", "ufw allow 10000:59999/tcp"},
			[]string{"ufw delete allow 7277/tcp", "ufw delete allow 443/udp", "ufw delete allow 7273/udp"}},
		"firewalld": {"firewalld", map[string]string{"firewall-cmd --permanent --query-port=443/tcp": "yes", "firewall-cmd --reload": "success",
			"firewall-cmd --permanent --add-port=7277/tcp": "success", "firewall-cmd --permanent --add-port=443/udp": "success", "firewall-cmd --permanent --add-port=7272/tcp": "success", "firewall-cmd --permanent --add-port=7273/udp": "success", "firewall-cmd --permanent --add-port=10000-59999/tcp": "success",
			"firewall-cmd --permanent --remove-port=7277/tcp": "success", "firewall-cmd --permanent --remove-port=443/udp": "success", "firewall-cmd --permanent --remove-port=7272/tcp": "success", "firewall-cmd --permanent --remove-port=7273/udp": "success", "firewall-cmd --permanent --remove-port=10000-59999/tcp": "success"},
			[]string{"7277/tcp", "443/udp", "7272/tcp", "7273/udp", "10000-59999/tcp"},
			[]string{"firewall-cmd --permanent --add-port=7277/tcp", "firewall-cmd --permanent --add-port=443/udp", "firewall-cmd --permanent --add-port=7273/udp", "firewall-cmd --reload"},
			[]string{"firewall-cmd --permanent --remove-port=7277/tcp", "firewall-cmd --permanent --remove-port=443/udp", "firewall-cmd --permanent --remove-port=7273/udp"}},
		"iptables": {"iptables", map[string]string{"iptables -I INPUT 1 " + tcp: "", "iptables -I INPUT 1 " + udp: "", "iptables -D INPUT " + tcp: "", "iptables -D INPUT " + udp: "",
			"iptables -S INPUT": "-P INPUT DROP\n-A INPUT " + tcp + "\n-A INPUT " + udp + "\n"},
			[]string{"iptables " + tcp, "iptables " + udp},
			[]string{"iptables -C INPUT " + tcp, "iptables -I INPUT 1 -p tcp -m multiport --dports 7277,443,7272,10000:59999 ", "iptables -I INPUT 1 -p udp -m multiport --dports 443,7273 "},
			[]string{"iptables -D INPUT -p tcp -m multiport --dports 7277,443,7272,10000:59999 ", "iptables -D INPUT -p udp -m multiport --dports 443,7273 "}},
		"nft": {"nft", map[string]string{"nft list ruleset": "table inet filter {\n\tchain input {\n\t\ttype filter hook input priority filter; policy drop;\n\t}\n}\n", "nft list chain inet filter input": "",
			"nft insert rule inet filter input " + nftTCP: "", "nft insert rule inet filter input " + nftUDP: "",
			"nft -a list ruleset":                        "table inet filter { # handle 1\n\tchain input { # handle 1\n\t\ttype filter hook input priority filter; policy drop;\n\t\t" + nftTCP + " # handle 7\n\t\t" + nftUDP + " # handle 8\n\t}\n}\n",
			"nft delete rule inet filter input handle 7": "", "nft delete rule inet filter input handle 8": ""}, []string{"nft"},
			[]string{"nft insert rule inet filter input " + nftTCP, "nft insert rule inet filter input " + nftUDP}, []string{"nft delete rule inet filter input handle 7", "nft delete rule inet filter input handle 8"}},
	} {
		f := newFakeHost(t, "60000 65535")
		f.answers = test.answers
		added, err := firewallAdd(t.Context(), f.run, test.kind, testRules)
		if err != nil || strings.Join(added, ",") != strings.Join(test.added, ",") {
			t.Fatalf("%s: added %v err %v, want %v", name, added, err, test.added)
		}
		for _, want := range test.wantAdd {
			if f.called(want) == 0 {
				t.Errorf("%s: %q did not run; calls %v", name, want, f.calls)
			}
		}
		f.calls = nil
		if err := firewallRemove(t.Context(), f.run, test.kind, added); err != nil {
			t.Fatalf("%s: remove: %v", name, err)
		}
		for _, want := range test.wantDel {
			if f.called(want) == 0 {
				t.Errorf("%s: %q did not run on remove; calls %v", name, want, f.calls)
			}
		}
	}
}

// A rule set that has only the TCP half is completed, a rule with other ports is not mistaken for ours,
// and a chain that cannot be listed is an error instead of a blind insert.
func TestNftMatchesTheExactRuleAndCompletesAMissingHalf(t *testing.T) {
	const ruleset = "table inet filter {\n\tchain input {\n\t\ttype filter hook input priority filter; policy drop;\n\t}\n}\n"
	const nftTCP = `tcp dport { 443, 7272, 7277, 10000-59999 } accept comment "paperboat-edge"`
	const nftUDP = `udp dport { 443, 7273 } accept comment "paperboat-edge"`
	f := newFakeHost(t, "60000 65535")
	f.answers = map[string]string{"nft list ruleset": ruleset, "nft list chain inet filter input": "\t\t" + nftTCP + "\n", "nft insert rule inet filter input " + nftUDP: ""}
	if _, err := firewallAdd(t.Context(), f.run, "nft", testRules); err != nil || f.called("nft insert rule inet filter input udp dport") != 1 || f.called("nft insert rule inet filter input tcp dport") != 0 {
		t.Fatalf("err %v calls %v", err, f.calls)
	}
	// An old rule without the claim port is not the rule we need.
	f = newFakeHost(t, "60000 65535")
	f.answers = map[string]string{"nft list ruleset": ruleset, "nft list chain inet filter input": "\t\ttcp dport { 443, 7272, 10000-59999 } accept comment \"paperboat-edge\"\n\t\t" + nftUDP + "\n", "nft insert rule inet filter input " + nftTCP: ""}
	if _, err := firewallAdd(t.Context(), f.run, "nft", testRules); err != nil || f.called("nft insert rule inet filter input tcp dport") != 1 {
		t.Fatalf("err %v calls %v", err, f.calls)
	}
	f = newFakeHost(t, "60000 65535")
	f.answers = map[string]string{"nft list ruleset": ruleset}
	if _, err := firewallAdd(t.Context(), f.run, "nft", testRules); err == nil || f.called("nft insert") != 0 {
		t.Fatalf("a chain that cannot be listed was inserted into blindly: err %v calls %v", err, f.calls)
	}
}

// Every backend reports the TCP and the UDP rules, so doctor flags a host that has only the TCP half.
func TestFirewallRulePresentNeedsTheUDPRulesToo(t *testing.T) {
	tcp := strings.Join(iptablesSpec("tcp", portsOf(testRules, "tcp")), " ")
	f := newFakeHost(t, "60000 65535")
	f.answers = map[string]string{"iptables -C INPUT " + tcp: ""}
	if firewallRulePresent(t.Context(), f.run, "iptables", testRules) {
		t.Fatal("iptables with only the TCP rule counted as open")
	}
	f.answers["iptables -C INPUT "+strings.Join(iptablesSpec("udp", portsOf(testRules, "udp")), " ")] = ""
	if !firewallRulePresent(t.Context(), f.run, "iptables", testRules) {
		t.Fatal("iptables with both rules not counted as open")
	}
	f.answers = map[string]string{"ufw status": "7277/tcp ALLOW Anywhere\n443/tcp ALLOW Anywhere\n7272/tcp ALLOW Anywhere\n10000:59999/tcp ALLOW Anywhere\n443/udp ALLOW Anywhere\n"}
	if firewallRulePresent(t.Context(), f.run, "ufw", testRules) {
		t.Fatal("ufw without the UDP carrier port counted as open")
	}
	f.answers["ufw status"] += "7273/udp ALLOW Anywhere\n"
	if !firewallRulePresent(t.Context(), f.run, "ufw", testRules) {
		t.Fatal("ufw with every rule not counted as open")
	}
}

func TestIptablesRuleAlreadyPresentIsNotAddedOrRemoved(t *testing.T) {
	tcp := strings.Join(iptablesSpec("tcp", portsOf(testRules, "tcp")), " ")
	udp := strings.Join(iptablesSpec("udp", portsOf(testRules, "udp")), " ")
	f := newFakeHost(t, "60000 65535")
	f.answers = map[string]string{"iptables -C INPUT " + tcp: "", "iptables -C INPUT " + udp: ""}
	added, err := firewallAdd(t.Context(), f.run, "iptables", testRules)
	if err != nil || len(added) != 0 || f.called("iptables -I") != 0 {
		t.Fatalf("added %v err %v calls %v", added, err, f.calls)
	}
}

// A runtime-only firewall (iptables, nft) is rebuilt on every start from the recorded rules, both protocols;
// the rules it recreates are recorded so uninstall removes them; ufw and firewalld keep their own.
func TestReapplyRebuildsBothProtocolsOnlyForRuntimeOnlyFirewalls(t *testing.T) {
	const ruleset = "table inet filter {\n\tchain input {\n\t\ttype filter hook input priority filter; policy drop;\n\t}\n}\n"
	tcp := strings.Join(iptablesSpec("tcp", portsOf(testRules, "tcp")), " ")
	udp := strings.Join(iptablesSpec("udp", portsOf(testRules, "udp")), " ")
	for kind, want := range map[string]int{"iptables": 2, "nft": 2, "ufw": 0, "firewalld": 0, "": 0} {
		f := newFakeHost(t, "60000 65535")
		f.answers = map[string]string{"iptables -I INPUT 1 " + tcp: "", "iptables -I INPUT 1 " + udp: "", "nft list ruleset": ruleset, "nft list chain inet filter input": "",
			`nft insert rule inet filter input tcp dport { 443, 7272, 7277, 10000-59999 } accept comment "paperboat-edge"`: "", `nft insert rule inet filter input udp dport { 443, 7273 } accept comment "paperboat-edge"`: ""}
		if err := writeHostnetRecord(f.paths.record, hostnetRecord{Firewall: kind, Rules: testRules}); err != nil {
			t.Fatal(err)
		}
		reapplyHostFirewall(t.Context(), f.run, f.paths, &strings.Builder{})
		if got := f.called("iptables -I INPUT 1") + f.called("nft insert rule"); got != want {
			t.Errorf("%q: %d rules re-opened, want %d; calls %v", kind, got, want, f.calls)
		}
		if record, _ := readHostnetRecord(f.paths.record); (want > 0) != (len(record.Added) > 0) {
			t.Errorf("%q: recreated rules not recorded for uninstall: %+v", kind, record.Added)
		}
	}
	// A record without rules is reported, not turned into an empty rule.
	f := newFakeHost(t, "60000 65535")
	var out strings.Builder
	if err := writeHostnetRecord(f.paths.record, hostnetRecord{Firewall: "iptables"}); err != nil {
		t.Fatal(err)
	}
	reapplyHostFirewall(t.Context(), f.run, f.paths, &out)
	if len(f.calls) != 0 || !strings.Contains(out.String(), "no firewall rules") {
		t.Fatalf("calls %v, output %q", f.calls, out.String())
	}
}

// The per-address connection limits are one nftables table pbh applies on install, re-applies on every
// service start and deletes on uninstall; a host without nft installs without them and is told.
func TestEdgeGuardAppliedReappliedAndRemoved(t *testing.T) {
	f := newFakeHost(t, "60000 65535")
	apply := "nft -f " + f.paths.guard
	list := "nft list table inet " + edgeGuardTable
	f.answers[apply] = ""
	f.answers["ip -4 -o route show default"] = "default via 10.0.0.1 dev ens3 proto dhcp src 10.0.0.5 metric 100\n"
	var out strings.Builder
	if err := setupHostNetwork(t.Context(), f.run, f.paths, &out, testConfig); err != nil {
		t.Fatal(err)
	}
	rules, _ := os.ReadFile(f.paths.guard)
	for _, want := range []string{"table inet " + edgeGuardTable, `iifname != { "ens3" } accept`, "th dport { 443, 7272-7275, 10000-59999 }", "limit rate over 100/second burst 200 packets", "ct count over 512", "ip6 saddr and ffff:ffff:ffff:ffff::"} {
		if !strings.Contains(string(rules), want) {
			t.Fatalf("guard rules miss %q:\n%s", want, rules)
		}
	}
	if f.called(apply) != 1 || !strings.Contains(out.String(), "Per-address connection limits") {
		t.Fatalf("guard not applied: %v\n%s", f.calls, out.String())
	}
	reapplyHostFirewall(t.Context(), f.run, f.paths, &out)
	if f.called(apply) != 2 {
		t.Fatalf("guard not re-applied on start: %v", f.calls)
	}
	f.answers[list] = "table inet " + edgeGuardTable
	f.answers["nft delete table inet "+edgeGuardTable] = ""
	if err := removeHostNetwork(t.Context(), f.run, f.paths, &out); err != nil {
		t.Fatal(err)
	}
	if f.called("nft delete table inet "+edgeGuardTable) != 1 {
		t.Fatalf("guard not removed: %v", f.calls)
	}

	bare := newFakeHost(t, "60000 65535")
	bare.answers["ip -4 -o route show default"] = "default via 10.0.0.1 dev ens3\n"
	out.Reset()
	if err := setupHostNetwork(t.Context(), bare.run, bare.paths, &out, testConfig); err != nil {
		t.Fatalf("install failed without nft: %v", err)
	}
	if record, _ := readHostnetRecord(bare.paths.record); record.Guard || !strings.Contains(out.String(), "nftables (nft) is not installed") {
		t.Fatalf("record %+v, output %q", record, out.String())
	}
}

// Only traffic arriving on a public interface (one with a default route) is limited: everything else
// (loopback, Docker/Podman/CNI/libvirt bridges, VPNs) is accepted before the jump to the limits, so
// local listeners in the port range never hit them. Interfaces come from both route families.
func TestEdgeGuardLimitsOnlyPublicInterfaces(t *testing.T) {
	f := newFakeHost(t, "60000 65535")
	f.answers["ip -4 -o route show default"] = "default via 10.0.0.1 dev enp0s6 proto dhcp src 10.0.0.221 metric 100\n"
	f.answers["ip -6 -o route show default"] = "default via fe80::1 dev enp0s6 proto ra metric 100\ndefault dev wan1@if7 metric 200\ndefault dev bad\"name metric 300\n"
	public := publicInterfaces(t.Context(), f.run)
	if strings.Join(public, ",") != "enp0s6,wan1" {
		t.Fatalf("public interfaces = %v", public)
	}
	rules := edgeGuardRules(public)
	jump := strings.Index(rules, "jump edge_new")
	exempt := strings.Index(rules, `iifname != { "enp0s6", "wan1" } accept`)
	if jump < 0 || exempt < 0 || exempt > jump {
		t.Fatalf("non-public interfaces are not accepted before the limits:\n%s", rules)
	}
	if skipped, err := applyEdgeGuard(t.Context(), newFakeHost(t, "60000 65535").run, f.paths.guard); err != nil || !strings.Contains(skipped, "default route") {
		t.Fatalf("a host with no default route: skipped=%q err=%v", skipped, err)
	}
}

// A relay-only node serves no public tunnel ports: it opens the claim port (the one in --claim-listen) and the
// relay's own ports, but neither moves the outgoing range, opens 10000-59999 nor applies the edge guard.
func TestSetupWithoutATunnelOpensOnlyTheRelayAndClaimPorts(t *testing.T) {
	f := newFakeHost(t, "32768 60999")
	f.answers["ufw status"] = "Status: active\n"
	for _, spec := range []string{"9000/tcp", "443/tcp", "443/udp", "7275/udp"} {
		f.answers["ufw allow "+spec] = "Rule added"
	}
	config := selfhost.Config{Listen: "[::]:9000", Components: []selfhost.Component{{Capability: "relay", TCPPort: 443, QUICPort: 7275}}}
	var out strings.Builder
	if err := setupHostNetwork(t.Context(), f.run, f.paths, &out, config); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if got, _ := currentOutgoing(f.paths.proc); got != "32768 60999" {
		t.Fatalf("outgoing range moved to %q on a node without a tunnel", got)
	}
	if f.called("ufw allow 10000:59999/tcp") != 0 || f.called("nft -f") != 0 || !strings.Contains(out.String(), "TCP 9000, 443 and UDP 443, 7275") {
		t.Fatalf("calls %v\n%s", f.calls, out.String())
	}
	record, _ := readHostnetRecord(f.paths.record)
	if len(record.Rules) != 4 || record.Guard {
		t.Fatalf("record %+v", record)
	}
}

// Uninstall finds the rules by their exact comment: with the record gone, in every table and chain, and beside
// rules of other owners (a jump target or a longer comment that merely contains the word), which stay.
func TestUninstallRemovesEveryCommentedRuleWithoutARecord(t *testing.T) {
	tcp := strings.Join(iptablesSpec("tcp", portsOf(testRules, "tcp")), " ")
	udp := strings.Join(iptablesSpec("udp", portsOf(testRules, "udp")), " ")
	foreign := "-p tcp -m tcp --dport 22 -m comment --comment other-owner -j ACCEPT"
	lookalikes := "-A INPUT -p tcp -m tcp --dport 23 -j paperboat-edge\n-A INPUT -p tcp -m tcp --dport 24 -m comment --comment \"allow paperboat-edge\" -j ACCEPT\n"
	f := newFakeHost(t, "60000 65535") // no hostnet.json: nothing recorded
	f.answers = map[string]string{
		"iptables -S INPUT":        "-P INPUT DROP\n-A INPUT " + tcp + "\n-A INPUT " + foreign + "\n" + lookalikes + "-A INPUT " + tcp + "\n-A INPUT " + udp + "\n",
		"ip6tables -S INPUT":       "-P INPUT DROP\n-A INPUT " + tcp + "\n",
		"iptables -D INPUT " + tcp: "", "iptables -D INPUT " + udp: "", "ip6tables -D INPUT " + tcp: "",
		"nft -a list ruleset": "table inet filter { # handle 1\n\tchain input { # handle 1\n\t\ttcp dport { 443 } accept comment \"paperboat-edge\" # handle 4\n\t\ttcp dport { 22 } accept comment \"other-owner\" # handle 5\n" +
			"\t\ttcp dport { 25 } accept comment \"allow paperboat-edge\" # handle 6\n\t}\n}\ntable ip nat { # handle 2\n\tchain extra { # handle 3\n\t\tudp dport { 7273 } accept comment \"paperboat-edge\" # handle 9\n\t}\n}\n",
		"nft delete rule inet filter input handle 4": "", "nft delete rule ip nat extra handle 9": "",
	}
	for _, path := range []string{f.paths.sysctl, f.paths.udp} {
		if err := os.WriteFile(path, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := removeHostNetwork(t.Context(), f.run, f.paths, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	// A rule listed twice is deleted once per listed line.
	for want, n := range map[string]int{"iptables -D INPUT " + tcp: 2, "iptables -D INPUT " + udp: 1, "ip6tables -D INPUT " + tcp: 1, "nft delete rule inet filter input handle 4": 1, "nft delete rule ip nat extra handle 9": 1} {
		if f.called(want) != n {
			t.Errorf("%q ran %d times, want %d; calls %v", want, f.called(want), n, f.calls)
		}
	}
	for _, kept := range []string{"iptables -D INPUT -p tcp -m tcp --dport 2", "nft delete rule inet filter input handle 5", "nft delete rule inet filter input handle 6"} {
		if f.called(kept) != 0 {
			t.Errorf("a rule of another owner was deleted (%q): %v", kept, f.calls)
		}
	}
	deletes := f.called("iptables -D") + f.called("ip6tables -D") + f.called("nft delete")
	if deletes != 6 {
		t.Errorf("%d delete commands ran, want exactly the 6 owned rules: %v", deletes, f.calls)
	}
	for _, gone := range []string{f.paths.sysctl, f.paths.udp} {
		if _, err := os.Stat(gone); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived uninstall without a record: %v", gone, err)
		}
	}
}

// iptables quotes values with spaces: a foreign comment that contains our marker as words is not our rule, and
// a delete replays the decoded arguments.
func TestSplitIptablesLineDecodesQuotedArguments(t *testing.T) {
	line := `-A INPUT -p tcp -m comment --comment "note --comment paperboat-edge here" -j ACCEPT`
	args, _ := splitIptablesLine(line)
	want := []string{"-A", "INPUT", "-p", "tcp", "-m", "comment", "--comment", "note --comment paperboat-edge here", "-j", "ACCEPT"}
	if strings.Join(args, "|") != strings.Join(want, "|") || ownedIptablesRule(args) {
		t.Fatalf("args %q owned=%v", args, ownedIptablesRule(args))
	}
	own, _ := splitIptablesLine("-A INPUT -p udp -m multiport --dports 443,7273 -m comment --comment paperboat-edge -j ACCEPT")
	if !ownedIptablesRule(own) || len(own) != 14 {
		t.Fatalf("own rule args %q", own)
	}
	if got, ok := splitIptablesLine(`-A INPUT --comment "a \"b\" c" -j ACCEPT`); !ok || got[3] != `a "b" c` {
		t.Fatalf("escaped quotes: %q", got)
	}
	if _, ok := splitIptablesLine(`-A INPUT --comment "unbalanced -j ACCEPT`); ok {
		t.Fatal("an open quote was accepted")
	}
}

// Listing failures other than a missing tool are errors for both iptables families and nft; a kernel without
// IPv6 filtering is not.
func TestUninstallReportsEveryKindOfFailedListing(t *testing.T) {
	for name, test := range map[string]struct {
		kind, failing, message string
		wantErr                bool
	}{
		"ip6tables permission":    {"iptables", "ip6tables", "Permission denied", true},
		"ip6tables no ipv6":       {"iptables", "ip6tables", "can't initialize ip6tables table `filter': Table does not exist", false},
		"nft permission":          {"nft", "nft", "Operation not permitted", true},
		"iptables backend dead":   {"iptables", "iptables", "Resource temporarily unavailable", true},
		"ip6tables legacy denied": {"iptables", "ip6tables", "ip6tables v1.8: can't initialize ip6tables table `filter': Permission denied (you must be root)", true},
	} {
		f := newFakeHost(t, "60000 65535")
		f.answers = map[string]string{"iptables -S INPUT": "-P INPUT DROP\n", "ip6tables -S INPUT": "-P INPUT DROP\n"}
		run := func(ctx context.Context, cmd string, args ...string) (string, error) {
			if cmd == test.failing && (args[0] == "-S" || args[0] == "-a") {
				return "", errors.New(test.message)
			}
			return f.run(ctx, cmd, args...)
		}
		err := firewallRemove(t.Context(), run, test.kind, nil)
		if (err != nil) != test.wantErr {
			t.Errorf("%s: err %v, want error=%v", name, err, test.wantErr)
		}
	}
}

// A listing that fails for a reason other than a missing tool is an error, and the record stays for a rerun.
func TestUninstallDoesNotReportSuccessWhenTheRuleListFails(t *testing.T) {
	f := newFakeHost(t, "60000 65535")
	if err := writeHostnetRecord(f.paths.record, hostnetRecord{Firewall: "iptables", Rules: testRules}); err != nil {
		t.Fatal(err)
	}
	failing := func(ctx context.Context, name string, args ...string) (string, error) {
		if name == "iptables" && args[0] == "-S" {
			return "", errors.New("iptables: Permission denied (you must be root)")
		}
		return f.run(ctx, name, args...)
	}
	err := removeHostNetwork(t.Context(), failing, f.paths, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "Permission denied") {
		t.Fatalf("err %v", err)
	}
	if _, statErr := os.Stat(f.paths.record); statErr != nil {
		t.Fatalf("the record was dropped although rules may remain: %v", statErr)
	}
}

// A rule listing with an open quote cannot be read safely: it is an error, nothing is deleted, the record stays.
func TestUninstallRefusesAnUnbalancedRuleListing(t *testing.T) {
	f := newFakeHost(t, "60000 65535")
	f.answers = map[string]string{"iptables -S INPUT": "-P INPUT DROP\n-A INPUT -m comment --comment \"broken -j ACCEPT\n-A INPUT -p tcp -m multiport --dports 443 -m comment --comment paperboat-edge -j ACCEPT\n"}
	err := firewallRemove(t.Context(), f.run, "iptables", nil)
	if err == nil || !strings.Contains(err.Error(), "unbalanced quote") {
		t.Fatalf("err %v", err)
	}
	if f.called("iptables -D") != 1 { // the readable owned rule is still removed; the broken line is reported
		t.Fatalf("calls %v", f.calls)
	}
}

// A host can filter IPv4 and IPv6 in separate nft tables: the rules go into every input chain with policy
// drop, and the host counts as open only when each of them has both.
func TestNftAddsAndChecksEveryInputChain(t *testing.T) {
	const ruleset = "table ip filter {\n\tchain input {\n\t\ttype filter hook input priority filter; policy drop;\n\t}\n}\n" +
		"table ip6 filter {\n\tchain input {\n\t\ttype filter hook input priority filter; policy drop;\n\t}\n}\n" +
		"table inet other {\n\tchain input {\n\t\ttype filter hook input priority filter; policy accept;\n\t}\n}\n"
	const nftTCP = `tcp dport { 443, 7272, 7277, 10000-59999 } accept comment "paperboat-edge"`
	const nftUDP = `udp dport { 443, 7273 } accept comment "paperboat-edge"`
	f := newFakeHost(t, "60000 65535")
	f.answers = map[string]string{"nft list ruleset": ruleset, "nft list chain ip filter input": "", "nft list chain ip6 filter input": "\t\t" + nftTCP + "\n"}
	for _, chain := range []string{"ip filter input", "ip6 filter input"} {
		f.answers["nft insert rule "+chain+" "+nftTCP] = ""
		f.answers["nft insert rule "+chain+" "+nftUDP] = ""
	}
	added, err := firewallAdd(t.Context(), f.run, "nft", testRules)
	if err != nil || len(added) != 1 {
		t.Fatalf("added %v err %v", added, err)
	}
	// ip: both rules; ip6: only the missing UDP rule; the accept-policy chain is left alone.
	for want, n := range map[string]int{"nft insert rule ip filter input tcp": 1, "nft insert rule ip filter input udp": 1, "nft insert rule ip6 filter input tcp": 0, "nft insert rule ip6 filter input udp": 1, "nft insert rule inet": 0} {
		if f.called(want) != n {
			t.Errorf("%q ran %d times, want %d; calls %v", want, f.called(want), n, f.calls)
		}
	}
	// Present only when every chain has both rules.
	f.answers["nft list chain ip filter input"] = "\t\t" + nftTCP + "\n\t\t" + nftUDP + "\n"
	if firewallRulePresent(t.Context(), f.run, "nft", testRules) {
		t.Fatal("the ip6 chain lacks the UDP rule but the host counted as open")
	}
	f.answers["nft list chain ip6 filter input"] = "\t\t" + nftTCP + "\n\t\t" + nftUDP + "\n"
	if !firewallRulePresent(t.Context(), f.run, "nft", testRules) {
		t.Fatal("every chain has both rules but the host did not count as open")
	}
}

// ufw answers once per address family: a rule is ours when either family was added, and doctor wants an ALLOW
// from anywhere for IPv4 and, where ufw lists IPv6 rules, for IPv6 too (not a DENY, not one limited to a source).
func TestUfwIPv6RulesAreTrackedAndChecked(t *testing.T) {
	f := newFakeHost(t, "60000 65535")
	f.answers = map[string]string{
		"ufw allow 7277/tcp": "Skipping adding existing rule\nRule added (v6)\n", "ufw allow 443/tcp": "Skipping adding existing rule\nSkipping adding existing rule (v6)\n",
		"ufw allow 443/udp": "Rule added\nRule added (v6)\n", "ufw allow 7272/tcp": "Rule added\n", "ufw allow 7273/udp": "Rule added\n", "ufw allow 10000:59999/tcp": "Rule added\nRule added (v6)\n",
	}
	added, err := firewallAdd(t.Context(), f.run, "ufw", testRules)
	if want := "7277/tcp,443/udp,7272/tcp,7273/udp,10000:59999/tcp"; err != nil || strings.Join(added, ",") != want {
		t.Fatalf("added %v err %v, want %s (a rule with only the v6 half new is ours, an existing one is not)", added, err, want)
	}
	rules := []firewallRule{{"tcp", "443"}, {"udp", "7273"}}
	status := func(lines ...string) map[string]string {
		return map[string]string{"ufw status": strings.Join(lines, "\n") + "\n"}
	}
	for name, test := range map[string]struct {
		status map[string]string
		want   bool
	}{
		"v4 only host":        {status("443/tcp ALLOW Anywhere", "7273/udp ALLOW Anywhere"), true},
		"v6 missing one":      {status("443/tcp ALLOW Anywhere", "7273/udp ALLOW Anywhere", "443/tcp (v6) ALLOW Anywhere (v6)"), false},
		"both families":       {status("443/tcp ALLOW Anywhere", "7273/udp ALLOW Anywhere", "443/tcp (v6) ALLOW Anywhere (v6)", "7273/udp (v6) ALLOW Anywhere (v6)"), true},
		"denied":              {status("443/tcp DENY Anywhere", "7273/udp ALLOW Anywhere"), false},
		"limited to a source": {status("443/tcp ALLOW 10.0.0.0/8", "7273/udp ALLOW Anywhere"), false},
		"direction in":        {status("443/tcp ALLOW IN Anywhere", "7273/udp ALLOW IN Anywhere"), true},
	} {
		f := newFakeHost(t, "60000 65535")
		f.answers = test.status
		if got := firewallRulePresent(t.Context(), f.run, "ufw", rules); got != test.want {
			t.Errorf("%s: present %v, want %v", name, got, test.want)
		}
	}
}
