package main

var machineHelpTexts = map[string]helpText{
	"pb machine": {
		Long: `List and manage the machines enrolled in your account: print the command that enrolls a new
one (add), rename it, choose whether it may sleep (availability), choose which incoming services
it accepts (capabilities), tag it, or revoke it. These commands name any enrolled machine by name
or ID; pb setup is what configures the machine you are sitting at. Revoking disconnects the
enrollment, it does not just hide the machine from the list.

In an interactive terminal, pb machine with no subcommand opens the machine picker. Elsewhere it
prints this help.`,
		Example: `
  pb machine list
  pb machine add --name db-1
  pb machine revoke old-laptop`},
	"pb machine list": {
		Long: `List enrolled machines, one per line: name, ownership (personal or team:<team>), state, your
permissions on it, its tags and its ID. Filter with --tag (repeat it to require several tags),
--owner, --q and --state. The list stops at --limit unless you pass --all, and says how many
more exist.

With --json the output is {"version", "machines", "total"}.`,
		Example: `
  pb machine list
  pb machine list --tag prod --tag ci
  pb machine list --state offline --json`},
	"pb machine add": {
		Long: `Print the one-shot commands that enroll a new machine: one for Linux and macOS, one for
Windows. Run the printed command on the new machine soon after: the enrollment token in it is
single-use and expires (--json shows expires_at). With --workspace the token
is for that workspace. --name suggests the machine's hostname.

If the printed commands cannot be shown, the unused enrollment is cancelled again.`,
		Example: `
  pb machine add
  pb machine add --name db-1
  pb machine add --workspace acme --json`},
	"pb machine rename": {
		Long: `Rename a machine, selected by its current name or ID. The new name must follow the machine
name rules; a refused name is explained without echoing what you typed. Its ID and grants
stay as they are. Afterwards the local machine list refreshes, so the new name resolves at once.`,
		Example: `
  pb machine rename old-name db-1
  pb machine rename m_abc123 db-1 --json`},
	"pb machine revoke": {
		Long: `Disconnect a machine and revoke its enrollment: its credentials lose authority and it
leaves your list. Revoking is not undone; the machine has to be enrolled again.

` + confirmNote + `

Revoking a machine that is already disconnected reports success. If the device was lost or
compromised, rotate your ENV keys afterwards.`,
		Example: `
  pb machine revoke old-laptop
  pb machine revoke old-laptop --confirm K3M9QX --json`},
	"pb machine availability": {
		Long: `Choose whether a machine may sleep. --mode allow-sleep lets it sleep when idle; --mode
keep-awake keeps it available while idle, which can raise battery use and heat and keep a laptop
awake with the lid closed, so it needs a confirmation (--confirm, below).


The setting is saved first. The command then waits up to 5 seconds for the machine to report
that it applied it and says either "applied" or "saved; application is pending" (for example
while the machine is offline).

` + confirmNote + `(Only keep-awake needs it.)`,
		Example: `
  pb machine availability db --mode keep-awake
  pb machine availability db --mode keep-awake --confirm K3M9QX
  pb machine availability db --mode allow-sleep`},
	"pb machine capabilities": {
		Long: `Choose which incoming services a machine accepts. Each flag is true or false; at least one
is required and the others stay as they are:

  --terminal        Paperboat terminal and pb exec
  --managed-ssh     managed SSH, SCP, SFTP and rsync
  --file-receive    files sent to its Inbox with pb send
  --preview-tunnel  serving previews and tunnels

A disabled service stays unavailable even while the machine is online. The command saves the
setting and reports how far it has been applied.`,
		Example: `
  pb machine capabilities db --terminal=true --managed-ssh=true
  pb machine capabilities db --file-receive=false`},
	"pb access": {
		Long: `Open private access to something that is never published: a service on one of your
enrolled machines (machine) or an existing private tunnel route (tunnel). Each command opens a
listener on a loopback address of this computer and runs in the foreground; Ctrl-C closes it
and the access ends with it. Nothing here makes a public URL; public access is pb tunnel and
pb preview. Access is authorized for the exact machine, port or route, and team machines also
need the matching grant.`,
		Example: `
  pb access machine db --port 5432
  pb access tunnel myroute --listen 127.0.0.1:8080`},
	"pb access machine": {
		Long: `Forward a loopback address on this computer to one TCP port of an enrolled machine, for
example a database that only listens on that machine. The command runs in the foreground, prints
the address it listens on, and stops on Ctrl-C (exit status 130). Only this computer can reach
the listener, up to 128 connections at once.

--port is required. --listen must be a literal loopback address, 127.0.0.1 or ::1 with an
optional port; the default 127.0.0.1:0 picks a free port. The target machine must offer the
service and its connection must be authorized. For a team machine that includes a grant of
private_access.`,
		Example: `
  pb access machine db --port 5432
  pb access machine db --port 6379 --listen 127.0.0.1:6379
  pb access machine db --port 5432 --json`},
	"pb access tunnel": {
		Long: `Open private TCP access to an existing private tunnel route, selected by tunnel or route ID.
The command opens a loopback listener on this computer, prints its address and keeps the access
alive with periodic heartbeats; the access is released when the command ends or stops
heartbeating.

--listen must be a literal loopback address, 127.0.0.1 or ::1 with an optional port; the
default 127.0.0.1:0 picks a free port.`,
		Example: `
  pb access tunnel myroute
  pb access tunnel tun_abc123 --listen 127.0.0.1:8080`},
	"pb relay": {
		Long: `Inspect the relays this computer can use. A relay carries traffic between machines when
they cannot reach each other directly. The list combines the relays Paperboat runs with the
self-hosted relays your account has selected, and shows how long this machine took to reach
each one.`,
		Example: `
  pb relay list
  pb relay list --json`},
	"pb relay list": {
		Long: `List the relay candidates this computer can use: the relays from the signed network
authority and the self-hosted relays your account has selected. In mixed pool mode both appear.
In self-hosted-only mode only the selected self-hosted relays appear, including unavailable
ones, and Paperboat's relays are never used instead.

STATUS is reported_ready when a signed control-plane observation of the relay is under 15
seconds old; it is not a live connection test. LATENCY is the connect time this machine last
measured to the relay, as its daemon reported it (measured about every 15 minutes, shown while
under an hour old). It is "-" until a measurement exists. The numbers are for display and never
choose a path.

Sign-in and the local daemon are required. With --json the output is paperboat.relay-list/v1:
the pool mode and the relays, each with latency_ms and latency_measured_at once measured.`,
		Example: `
  pb relay list
  pb relay list --json`},
	"pb edge": {
		Long: `Inspect the tunnel edges that serve the active workspace: edges Paperboat runs, or the
self-hosted edges you selected. Listing an edge never grants a route admission.`,
		Example: `
  pb edge list
  pb edge list --workspace acme --json`},
}
