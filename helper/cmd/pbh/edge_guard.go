package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/pinksaucepasta/paperboat/shared/edgeports"
)

// The edge guard is the kernel's per-address connection limit on the public edge ports (edge abuse
// plan, layer L1): it drops a source opening more than 100 connections a second (burst 200) and
// resets a source holding more than 512, before the edge process spends anything on them. IPv4
// sources count whole, IPv6 by /64. pbh owns one nftables table for it, applies it on install and
// on every service start (nftables rules do not survive a reboot), and deletes it on uninstall.
// Managed edges are pbh installations, so they get the same rules. Prior art: the nftables wiki's
// "meters" per-source rate and connlimit examples.

const edgeGuardTable = "paperboat_edge_guard"

// edgeGuardRules replaces the table atomically: the empty declaration makes the delete safe when
// the table is absent, and nft applies the file as one transaction.
func edgeGuardRules() string {
	return fmt.Sprintf(`table inet %[1]s {}
delete table inet %[1]s
table inet %[1]s {
	set new4 { type ipv4_addr; flags dynamic, timeout; timeout 1m; size 65536; }
	set new6 { type ipv6_addr; flags dynamic, timeout; timeout 1m; size 65536; }
	set held4 { type ipv4_addr; flags dynamic; size 65536; }
	set held6 { type ipv6_addr; flags dynamic; size 65536; }
	chain input {
		type filter hook input priority filter - 10; policy accept;
		meta l4proto { tcp, udp } th dport { 443, %[2]d-%[3]d } ct state new jump edge_new
	}
	chain edge_new {
		add @new4 { ip saddr limit rate over 100/second burst 200 packets } drop
		add @new6 { ip6 saddr and ffff:ffff:ffff:ffff:: limit rate over 100/second burst 200 packets } drop
		meta l4proto tcp add @held4 { ip saddr ct count over 512 } reject with tcp reset
		meta l4proto tcp add @held6 { ip6 saddr and ffff:ffff:ffff:ffff:: ct count over 512 } reject with tcp reset
	}
}
`, edgeGuardTable, edgeports.PublicTCPMin, edgeports.PublicTCPMax)
}

// applyEdgeGuard loads the guard table. It reports false, with no error, on a host without nft.
func applyEdgeGuard(ctx context.Context, run commander, path string) (bool, error) {
	if err := os.WriteFile(path, []byte(edgeGuardRules()), 0o600); err != nil {
		return false, err
	}
	if _, err := run(ctx, "nft", "-f", path); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
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
	applied, err := applyEdgeGuard(ctx, run, paths.guard)
	if err != nil {
		return fmt.Errorf("%w: %w", commandError("applying the per-address connection limits (nftables table "+edgeGuardTable+") failed; rerun pbh install as root"), err)
	}
	record.Guard = applied
	if applied {
		fmt.Fprintln(out, "Per-address connection limits on TCP/UDP 443 and the public TCP range: on (nftables).")
	} else {
		fmt.Fprintln(out, "nftables (nft) is not installed: per-address connection limits are off. Install nftables and rerun pbh install to turn them on.")
	}
	return nil
}
