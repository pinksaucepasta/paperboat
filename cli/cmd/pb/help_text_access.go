package main

var accessHelpTexts = map[string]helpText{
	"pb domain add": {
		Long: `Add a domain you control to the active workspace and print the DNS records to create at your
DNS provider: a CNAME that proves you control it, and the records that point names at
Paperboat. Then run pb domain verify. Once the domain is verified, any name one level under it
works with --domain on tunnels and previews.`,
		Example: `
  pb domain add dev.example.com
  pb domain add dev.example.com --json`},
	"pb domain list": {
		Long: `List the workspace's domains with their DNS verification state, certificate state and how many
tunnels and previews use each. With --json the output is {"items": [...]}.`,
		Example: `
  pb domain list
  pb domain list --json`},
	"pb domain show": {
		Long: `Show one domain: its DNS records, verification state, certificate state and expiry, the fix
for a failed check, and the tunnels and previews that use it. Name it by hostname or ID; case
and a trailing dot are ignored.`,
		Example: `
  pb domain show dev.example.com
  pb domain show dev.example.com --json`},
	"pb domain verify": {Example: `
  pb domain verify dev.example.com
  pb domain verify dev.example.com --timeout 10m`},
	"pb domain remove": {
		Long: `Remove a domain from the workspace. A domain that a tunnel or preview still uses is refused,
and the message names the hostnames to remove first (pb tunnel domain remove). DNS records at
your provider are not touched; delete them there.

` + confirmNote,
		Example: `
  pb domain remove dev.example.com
  pb domain remove dev.example.com --confirm A1B2C3`},
	"pb port name list": {Example: `
  pb port name list
  pb port name list --json`},
	"pb ssh": {Example: `
  pb ssh homelab
  pb ssh deploy@homelab
  pb ssh homelab -- -L 8080:localhost:80`},
	"pb ssh doctor": {Example: `
  pb ssh doctor homelab
  pb ssh doctor homelab --json`},
	"pb ssh trust-host": {
		Long: `Approve a pending SSH host-key change for a machine. Paperboat does not accept a changed
host identity on its own: check the pending SHA256 fingerprint out of band (for example on the
machine itself) before you trust it.

With --fingerprint the command trusts only if it equals the pending fingerprint; any other
value changes nothing. Without it, the command shows the current and pending fingerprints and
asks [y/N] in a terminal, and refuses outside one. --json needs --fingerprint. It does not run
OpenSSH.`,
		Example: `
  pb ssh trust-host homelab
  pb ssh trust-host homelab --fingerprint SHA256:abc123
  pb ssh trust-host homelab --fingerprint SHA256:abc123 --json`},
	"pb scp": {
		Long: `Run the system scp with Paperboat machine names. Every argument is passed to scp unchanged:
pb parses no flags of its own. An operand written [user@]machine:path is replaced with the
machine's managed SSH address; other operands (local paths, URLs, Windows drive paths) are left
alone. At least one operand must name a machine. Host identity checks stay on, and you must be
signed in with the machine paired.`,
		Example: `
  pb scp homelab:/var/log/app.log .
  pb scp ./bundle.tar.gz deploy@homelab:/srv/app/
  pb scp -r homelab:/srv/data ./backup/`},
	"pb sftp": {
		Long: `Run the system sftp with Paperboat machine names. Every argument is passed to sftp
unchanged; pb parses no flags of its own. The destination is the first argument that is not an
option (after -- if present), written [user@]machine with an optional :path, and it is replaced
with the machine's managed SSH address. Host identity checks stay on.`,
		Example: `
  pb sftp homelab
  pb sftp deploy@homelab:/srv/app
  pb sftp -P 2222 homelab`},
	"pb rsync": {
		Long: `Run the system rsync with Paperboat machine names. Every argument is passed to rsync
unchanged; pb parses no flags of its own. An operand written [user@]machine:path is replaced
with the machine's managed SSH address; at least one operand must name a machine. A drive-letter
path on a Windows machine is converted to the absolute form rsync reads. What is copied or
deleted, and the exit status, are rsync's: read the command line before using --delete.`,
		Example: `
  pb rsync -avz homelab:/srv/app/ ./app/
  pb rsync -avz ./dist/ deploy@homelab:/srv/app/
  pb rsync -avz --delete homelab:C:/data/ ./data/`},
	"pb exec": {
		Long: `Run a command on a machine without a terminal session: pb exec <machine> -- <command...>.
The -- is required, and the arguments after it run exactly as given, with no shell unless you
name one. Output and the exit status come from the remote command.

--cwd sets the working directory and must be absolute in the machine's own form: /... on
Unix, a drive letter on Windows. --env NAME=value sets a variable (repeatable; valid names,
each once). --timeout bounds the whole run, including its start (at most 24h). --pty allocates
a remote terminal sized like this one (80x24 when unknown). --json prints
paperboat.exec-event/v1 events, one JSON object per line.

Ctrl-C cancels the remote command; a second Ctrl-C stops waiting and exits at once. A short
network break is reattached automatically.

Exit status: the remote command's own; 124 when --timeout ran out; 130 when cancelled; 125 when
the machine refused or could not start the command; 255 when the connection failed and the
command did not run.`,
		Example: `
  pb exec homelab -- uname -a
  pb exec homelab --cwd /srv/app -- systemctl restart app
  pb exec homelab --timeout 30s -- ./migrate.sh
  pb exec winbox --cwd 'C:\Deploy' -- setup.exe /quiet`},
	"pb connect": {
		Long: `Open a durable terminal session on a machine (an "environment") and attach this terminal to
it. Without --session it always creates a fresh session, optionally named with --name; a
session keeps running when you disconnect. Attach to an existing one with --session (its name
or ID), or with pb session attach. --name and --session cannot be combined. A second argument
new is accepted and means the same as leaving --session out; it cannot be combined with
--session.

The status bar shows connection and recording state: --status-bar auto, on or off,
--status-bar-fullscreen hide or show, --status-bar-theme terminal, dark, light or mono.
--debug shows the pb versions in use. If the connection drops, pb reconnects; at the start of a
line, Enter ~ r reconnects now, Enter ~ . disconnects, and ~~ sends a literal ~.`,
		Example: `
  pb connect homelab
  pb connect homelab --name deploy
  pb connect homelab --session deploy
  pb connect homelab --status-bar off`},
	"pb recipe list": {Example: "\n  pb recipe list\n  pb recipe list --json"},
	"pb recipe show": {Example: "\n  pb recipe show ci\n  pb recipe show ci --json"},
}
