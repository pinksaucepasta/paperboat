package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/pinksaucepasta/paperboat/shared/edgeports"
)

// The edge guard is the kernel's per-address connection limit on the public edge ports (edge abuse
// plan, layer L1): it drops a source opening more than 100 connections a second (burst 200) and
// resets a source holding more than 512, before the edge process spends anything on them. IPv4
// sources count whole, IPv6 by /64. Only traffic arriving on a public interface (one that carries a
// default route) counts: loopback, container and VM bridges (Docker, Podman, CNI, libvirt) and VPN
// interfaces are never limited, so local services in the port range are untouched. pbh owns one
// nftables table for it, applies it on install and on every service start (nftables rules do not
// survive a reboot, and the public interfaces are found again), and deletes it on uninstall. Managed
// edges are pbh installations, so they get the same rules. Prior art: the nftables wiki's "meters"
// per-source rate and connlimit examples.

const edgeGuardTable = "paperboat_edge_guard"

// edgeGuardRules replaces the table atomically: the empty declaration makes the delete safe when
// the table is absent, and nft applies the file as one transaction. public names the interfaces
// whose traffic is limited; it must not be empty.
func edgeGuardRules(public []string) string {
	quoted := make([]string, len(public))
	for i, name := range public {
		quoted[i] = `"` + name + `"`
	}
	return fmt.Sprintf(`table inet %[1]s {}
delete table inet %[1]s
table inet %[1]s {
	set new4 { type ipv4_addr; flags dynamic, timeout; timeout 1m; size 65536; }
	set new6 { type ipv6_addr; flags dynamic, timeout; timeout 1m; size 65536; }
	set held4 { type ipv4_addr; flags dynamic; size 65536; }
	set held6 { type ipv6_addr; flags dynamic; size 65536; }
	chain input {
		type filter hook input priority filter - 10; policy accept;
		iifname != { %[4]s } accept
		meta l4proto { tcp, udp } th dport { 443, %[2]d-%[3]d } ct state new jump edge_new
	}
	chain edge_new {
		add @new4 { ip saddr limit rate over 100/second burst 200 packets } drop
		add @new6 { ip6 saddr and ffff:ffff:ffff:ffff:: limit rate over 100/second burst 200 packets } drop
		meta l4proto tcp add @held4 { ip saddr ct count over 512 } reject with tcp reset
		meta l4proto tcp add @held6 { ip6 saddr and ffff:ffff:ffff:ffff:: ct count over 512 } reject with tcp reset
	}
}
`, edgeGuardTable, edgeports.PublicTCPMin, edgeports.PublicTCPMax, strings.Join(quoted, ", "))
}

// publicInterfaces names the interfaces that carry a default route (IPv4 or IPv6), the ones public
// traffic arrives on. Names outside the kernel's interface-name characters are skipped.
func publicInterfaces(ctx context.Context, run commander) []string {
	var names []string
	for _, family := range []string{"-4", "-6"} {
		out, err := run(ctx, "ip", family, "-o", "route", "show", "default")
		if err != nil {
			continue
		}
		for _, line := range strings.Split(out, "\n") {
			fields := strings.Fields(line)
			for i := 0; i+1 < len(fields); i++ {
				if fields[i] != "dev" {
					continue
				}
				name, _, _ := strings.Cut(fields[i+1], "@")
				if validInterfaceName(name) && !slices.Contains(names, name) {
					names = append(names, name)
				}
			}
		}
	}
	return names
}

func validInterfaceName(name string) bool {
	if name == "" || len(name) > 15 {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

// applyEdgeGuard loads the guard table. Without nft, or without a public interface to limit, it
// applies nothing and says why, with no error.
func applyEdgeGuard(ctx context.Context, run commander, path string) (skipped string, err error) {
	public := publicInterfaces(ctx, run)
	if len(public) == 0 {
		return "no interface carries a default route, so pbh cannot tell which traffic is public", nil
	}
	if err := os.WriteFile(path, []byte(edgeGuardRules(public)), 0o600); err != nil {
		return "", err
	}
	if _, err := run(ctx, "nft", "-f", path); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return "nftables (nft) is not installed; install it and rerun pbh install", nil
		}
		return "", err
	}
	return "", nil
}

func removeEdgeGuard(ctx context.Context, run commander) error {
	if !edgeGuardPresent(ctx, run) {
		return nil
	}
	_, err := run(ctx, "nft", "delete", "table", "inet", edgeGuardTable)
	return err
}

func edgeGuardPresent(ctx context.Context, run commander) bool {
	_, err := run(ctx, "nft", "list", "table", "inet", edgeGuardTable)
	return err == nil
}

// setupEdgeGuard applies the guard on install and records it; a host without nft is told what it
// misses rather than failing the install.
func setupEdgeGuard(ctx context.Context, run commander, paths hostPaths, record *hostnetRecord, out io.Writer) error {
	skipped, err := applyEdgeGuard(ctx, run, paths.guard)
	if err != nil {
		return fmt.Errorf("%w: %w", commandError("applying the per-address connection limits (nftables table "+edgeGuardTable+") failed; rerun pbh install as root"), err)
	}
	record.Guard = skipped == ""
	if record.Guard {
		fmt.Fprintln(out, "Per-address connection limits on TCP/UDP 443 and the public TCP range: on (nftables, public interfaces only).")
	} else {
		fmt.Fprintln(out, "Per-address connection limits are off: "+skipped+".")
	}
	return nil
}
