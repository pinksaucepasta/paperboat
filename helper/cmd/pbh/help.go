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
  doctor          check that this host can serve public tunnel ports
  uninstall       remove the service and containers
  serve           run the node in the foreground (the systemd unit runs this)
  check-host      check that this operating system and architecture are supported
  version         print the pbh version

Run pbh <command> --help for a command's description and flags.
`

// commandHelp is the paragraph printed above a command's flags by `pbh <command> --help`.
var commandHelp = map[string]string{
	"install": `Install a local Paperboat node (relay, tunnel or both) as the paperboat-selfhost systemd service.
Installation assigns no account or team: claim the node in Dashboard → Network → Add self-hosted
node with the address and code printed here (pbh selfhost code prints a new one). Re-running the
same command upgrades in place and keeps data. Needs root (it re-runs itself with sudo) unless
--no-service is given.
Examples:
  pbh install --enable both --endpoint-host edge.example.com`,
	"selfhost code": `Print this installation's claim address and a fresh single-use code (valid for 15 minutes) for
Dashboard → Network → Add self-hosted node. The code carries no account identity.`,
	"status": `Show the paperboat-selfhost service state (systemctl status). A running service is not
readiness; use pbh doctor for the host and Dashboard → Network for registration.`,
	"doctor": `Check this host for serving public tunnel ports: the outgoing port range, the host firewall for
TCP 443 and the public port range, per-address connection limits, and a reachability test of a
test port from the control plane. Each failed check names the fix. Installations without a tunnel
need no host setup and report so. Fails when no installation exists in --state-dir.`,
	"configure-tls": `Operator command for Paperboat-managed installations only; it refuses customer self-hosted nodes. Set or rotate the browser-trusted HTTPS certificate for this installation's endpoint hostname.
The certificate hostname, validity, key pairing and owner-only key permissions are checked before
anything is saved; the service then restarts, and the previous certificate is restored when it
cannot. Carrier identity and enrollment do not change. Rerun it after each certificate renewal.
Example:
  pbh configure-tls --https-cert /etc/paperboat/fullchain.pem --https-key /etc/paperboat/privkey.pem`,
	"uninstall": `Stop the service and remove its containers and the pbh executables. Protected identity,
credentials and configuration are kept for a reinstall unless both --remove-data and
--confirm-remove-data are given, which deletes them permanently. Remove the node from
Dashboard → Network to revoke its registration. Needs root (it re-runs itself with sudo).`,
	"serve": `Run the installed node in the foreground: reapply the host firewall rules, serve the claim API on
the address chosen at install, and supervise the relay and edge containers until SIGINT or
SIGTERM. The paperboat-selfhost systemd unit runs this; run it by hand only to debug.`,
}

func isHelpFlag(arg string) bool { return arg == "--help" || arg == "-h" || arg == "-help" }

// operatorFlags work only on managed installations and stay out of customer help.
var operatorFlags = map[string]bool{"https-cert": true, "https-key": true}

func printHelp(out io.Writer, name string, fs *flag.FlagSet) {
	fmt.Fprintf(out, "pbh %s: %s\n\nFlags:\n", name, commandHelp[name])
	shown := flag.NewFlagSet(name, flag.ContinueOnError)
	shown.SetOutput(out)
	fs.VisitAll(func(f *flag.Flag) {
		if name == "install" && operatorFlags[f.Name] {
			return
		}
		shown.Var(f.Value, f.Name, f.Usage)
	})
	shown.PrintDefaults()
}
