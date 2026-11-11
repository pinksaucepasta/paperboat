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
)

// fakeHost answers host commands from a table and records every call; a `sysctl -w` rewrites the
// outgoing-range file the way the kernel would.
type fakeHost struct {
	paths     hostPaths
	answers   map[string]string // command line -> output; a missing key is an error
	calls     []string
	sysctlOff bool // a sysctl that does not change the range (something overrides it)
}

func newFakeHost(t *testing.T, outgoing string) *fakeHost {
	dir := t.TempDir()
	f := &fakeHost{paths: hostPaths{sysctl: filepath.Join(dir, "sysctl.conf"), proc: filepath.Join(dir, "range"), record: filepath.Join(dir, "hostnet.json"), guard: filepath.Join(dir, "edge-guard.nft")}, answers: map[string]string{}}
	if err := os.WriteFile(f.paths.proc, []byte(outgoing+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fakeHost) run(_ context.Context, name string, args ...string) (string, error) {
	line := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, line)
	if name == "sysctl" {
		if !f.sysctlOff {
			_ = os.WriteFile(f.paths.proc, []byte(strings.TrimPrefix(args[1], "net.ipv4.ip_local_port_range=")+"\n"), 0o644)
		}
		return "", nil
	}
	if out, ok := f.answers[line]; ok {
		return out, nil
	}
	if name == "nft" {
		return "", fmt.Errorf("%s: %w", line, exec.ErrNotFound) // a host without nftables
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
	if err := setupHostNetwork(t.Context(), f.run, f.paths, &out); err != nil {
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
	for _, gone := range []string{f.paths.sysctl, f.paths.record} {
		if _, err := os.Stat(gone); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s survived uninstall: %v", gone, err)
		}
	}
}

func TestSetupLeavesACorrectRangeAlone(t *testing.T) {
	f := newFakeHost(t, "60000 65535")
	if err := os.WriteFile(f.paths.sysctl, []byte("net.ipv4.ip_local_port_range = 60000 65535\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := setupHostNetwork(t.Context(), f.run, f.paths, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if f.called("sysctl") != 0 {
		t.Fatalf("sysctl ran for an already correct range: %v", f.calls)
	}
}

func TestSetupRefusesWhenAnotherSysctlOverridesTheRange(t *testing.T) {
	f := newFakeHost(t, "32768 60999")
	f.sysctlOff = true
	err := setupHostNetwork(t.Context(), f.run, f.paths, &strings.Builder{})
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

func TestFirewallAddAndRemovePerKind(t *testing.T) {
	spec := strings.Join(iptablesSpec(), " ")
	for name, test := range map[string]struct {
		kind    string
		answers map[string]string
		added   []string
		wantAdd []string // command prefixes that must have run on add
		wantDel []string // ... and on remove
	}{
		"ufw": {"ufw", map[string]string{"ufw allow 443/tcp": "Rule added", "ufw allow 10000:59999/tcp": "Skipping adding existing rule", "ufw delete allow 443/tcp": ""}, []string{"443/tcp"},
			[]string{"ufw allow 443/tcp", "ufw allow 10000:59999/tcp"}, []string{"ufw delete allow 443/tcp"}},
		"firewalld": {"firewalld", map[string]string{"firewall-cmd --query-port=443/tcp": "yes", "firewall-cmd --query-port=10000-59999/tcp": "no", "firewall-cmd --permanent --add-port=10000-59999/tcp": "success", "firewall-cmd --reload": "success", "firewall-cmd --permanent --remove-port=10000-59999/tcp": "success"}, []string{"10000-59999/tcp"},
			[]string{"firewall-cmd --permanent --add-port=10000-59999/tcp", "firewall-cmd --reload"}, []string{"firewall-cmd --permanent --remove-port=10000-59999/tcp"}},
		"iptables": {"iptables", map[string]string{"iptables -I INPUT 1 " + spec: "", "iptables -D INPUT " + spec: ""}, []string{"iptables"},
			[]string{"iptables -C INPUT " + spec, "iptables -I INPUT 1 " + spec}, []string{"iptables -D INPUT " + spec}},
		"nft": {"nft", map[string]string{"nft list ruleset": "table inet filter {\n\tchain input {\n\t\ttype filter hook input priority filter; policy drop;\n\t}\n}\n", "nft insert rule inet filter input tcp dport { 443, 10000-59999 } accept comment \"paperboat-edge\"": "",
			"nft -a list chain inet filter input": "\t\ttcp dport { 443, 10000-59999 } accept comment \"paperboat-edge\" # handle 7\n", "nft delete rule inet filter input handle 7": ""}, []string{"nft"},
			[]string{"nft insert rule inet filter input"}, []string{"nft delete rule inet filter input handle 7"}},
	} {
		f := newFakeHost(t, "60000 65535")
		f.answers = test.answers
		added, err := firewallAdd(t.Context(), f.run, test.kind)
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

func TestIptablesRuleAlreadyPresentIsNotAddedOrRemoved(t *testing.T) {
	spec := strings.Join(iptablesSpec(), " ")
	f := newFakeHost(t, "60000 65535")
	f.answers = map[string]string{"iptables -C INPUT " + spec: ""}
	added, err := firewallAdd(t.Context(), f.run, "iptables")
	if err != nil || len(added) != 0 || f.called("iptables -I") != 0 {
		t.Fatalf("added %v err %v calls %v", added, err, f.calls)
	}
}

func TestReapplyOnlyForRuntimeOnlyFirewalls(t *testing.T) {
	spec := strings.Join(iptablesSpec(), " ")
	for kind, wantCalls := range map[string]bool{"iptables": true, "nft": true, "ufw": false, "firewalld": false, "": false} {
		f := newFakeHost(t, "60000 65535")
		f.answers = map[string]string{"iptables -I INPUT 1 " + spec: "", "nft list ruleset": "table inet filter {\n\tchain input {\n\t\ttype filter hook input priority filter; policy drop;\n\t}\n}\n", "nft insert rule inet filter input tcp dport { 443, 10000-59999 } accept comment \"paperboat-edge\"": ""}
		if err := writeHostnetRecord(f.paths.record, hostnetRecord{Firewall: kind}); err != nil {
			t.Fatal(err)
		}
		reapplyHostFirewall(t.Context(), f.run, f.paths, &strings.Builder{})
		if (len(f.calls) > 0) != wantCalls {
			t.Errorf("%q: calls %v, want calls=%v", kind, f.calls, wantCalls)
		}
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
	if err := setupHostNetwork(t.Context(), f.run, f.paths, &out); err != nil {
		t.Fatal(err)
	}
	rules, _ := os.ReadFile(f.paths.guard)
	for _, want := range []string{"table inet " + edgeGuardTable, `iifname != { "ens3" } accept`, "th dport { 443, 10000-59999 }", "limit rate over 100/second burst 200 packets", "ct count over 512", "ip6 saddr and ffff:ffff:ffff:ffff::"} {
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
	if err := setupHostNetwork(t.Context(), bare.run, bare.paths, &out); err != nil {
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
