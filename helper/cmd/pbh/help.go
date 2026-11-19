package main

import (
	"flag"
	"fmt"
	"io"
)

// topHelp is what `pbh help` prints: every command, including the ones the service runs itself.
const topHelp = `pbh installs and manages a self-hosted Paperboat relay and/or tunnel edge on this host.

Usage: pbh <command> [flags]

Commands:
  install         install or upgrade the node and its systemd service
  selfhost code   print a fresh single-use code to claim this node in the dashboard
  status          show the paperboat-selfhost service state
  logs            show the relay and edge logs (--follow, --since, --lines)
  restart         restart the service, or only the relay or the edge
  update          update pbh and the relay and edge to the latest signed release
  update-apply    run an update the dashboard requested (its Update button starts this)
  doctor          check that this host can serve public tunnel ports
  uninstall       remove the service and containers
  serve           run the node in the foreground (the systemd unit runs this)
  check-host      check that this operating system and architecture are supported
  version         print the pbh version

Run pbh <command> --help for a command's description and flags.
`

// commandHelp is the paragraph printed above a command's flags by `pbh <command> --help`.
var commandHelp = map[string]string{
	"install": `Install a local Paperboat node (--relay, --edge or --both) as the paperboat-selfhost systemd service.
Installation assigns no account or team: claim the node in Dashboard → Network → Add self-hosted
node with the address and code printed here (pbh selfhost code prints a new one). Re-running the
same command upgrades in place and keeps data. Needs root (it re-runs itself with sudo) unless
--no-service is given.
Examples:
  pbh install --both --endpoint-host edge.example.com`,
	"logs": `Show the logs of this installation's relay and edge containers. Name relay or edge for one; without a
name both are shown, each line marked with its component. --lines (default 200, at most 10000) and --since
(for example 10m) bound what is read; --follow keeps streaming until interrupted. Needs root (it re-runs
itself with sudo).
Examples:
  pbh logs --since 1h edge
  pbh logs --follow relay`,
	"restart": `Restart the paperboat-selfhost service (relay and edge), or only the relay or the edge when named. A named
component is stopped and the service starts it again; restart waits until it runs. Needs root (it re-runs
itself with sudo).
Examples:
  pbh restart
  pbh restart edge`,
	"update": `Update this host to the newest signed release, the same way the dashboard's Update button does: the
release is downloaded and verified against the signed release metadata before anything changes, and the
previous version is restored (with the exact error shown) when the new one fails its health check. Shows
current → latest and asks to confirm on a terminal; --yes skips the question. Needs root (it re-runs itself with sudo).`,
	"update-apply": `Run the update the dashboard requested for this node, reporting each step to it. The service starts this in
its own systemd unit; run pbh update to update by hand.`,
	"selfhost code": `Print this installation's claim address and a fresh single-use code (valid for 15 minutes) for
Dashboard → Network → Add self-hosted node. The code carries no account identity.`,
	"status": `Show the paperboat-selfhost service state (systemctl status). A running service is not
readiness; use pbh doctor for the host and Dashboard → Network for registration.`,
	"doctor": `Check this host for serving public tunnel ports: the outgoing port range, the host firewall for
TCP 443 and the public port range, per-address connection limits, and a reachability test of a
test port from the control plane. Each failed check names the fix. Installations without a tunnel
need no host setup and report so. Fails when no installation exists in --state-dir.`,
	"uninstall": `Stop the service and remove its containers and the pbh executables. Protected identity,
credentials and configuration are kept for a reinstall unless both --remove-data and
--confirm-remove-data are given, which deletes them permanently. Remove the node from
Dashboard → Network to revoke its registration. Needs root (it re-runs itself with sudo).`,
	"serve": `Run the installed node in the foreground: reapply the host firewall rules, serve the claim API on
the address chosen at install, and supervise the relay and edge containers until SIGINT or
SIGTERM. The paperboat-selfhost systemd unit runs this; run it by hand only to debug.`,
}

func isHelpFlag(arg string) bool { return arg == "--help" || arg == "-h" || arg == "-help" }

func printHelp(out io.Writer, name string, fs *flag.FlagSet) {
	fmt.Fprintf(out, "pbh %s: %s\n\nFlags:\n", name, commandHelp[name])
	fs.SetOutput(out)
	fs.PrintDefaults()
}
