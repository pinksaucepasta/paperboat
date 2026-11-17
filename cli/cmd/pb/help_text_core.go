package main

var coreHelpTexts = map[string]helpText{
	"pb config set": {
		Long: `Set one local configuration key of this CLI installation. The keys are server (the
Paperboat server URL: absolute http or https, without credentials, query or fragment) and
ssh-target-port (1-65535, the port registered for this machine's managed SSH target). Check the
effective value afterwards with pb config show; pb config unset server removes the setting.`,
		Example: `
  pb config set server https://api.example.test
  pb config set ssh-target-port 2222
  pb config set server https://api.example.test --json`},
	"pb config sync": {Example: `
  pb config sync path
  pb config sync validate
  pb config sync apply`},
	"pb config sync validate": {Example: `
  pb config sync validate
  pb config sync validate --repository config-repo --json`},
	"pb config sync apply": {Example: `
  pb config sync apply
  pb config sync apply --confirm A1B2C3`},
	"pb session rename": {Example: `
  pb session rename Studio sess-1234 demo`},
	"pb session close": {Example: `
  pb session close Studio sess-1234
  pb session close Studio --all
  pb session close Studio --all --confirm A1B2C3`},
	"pb session delete": {Example: `
  pb session delete Studio sess-1234
  pb session delete Studio --all --confirm A1B2C3`},
	"pb new": {Example: `
  pb new
  pb new --name demo`},
	"pb setup": {
		Long: `Enroll this machine in the signed-in account and set it up: create its Inbox, install the
running executable as the Paperboat service, and register its managed SSH target. Sign in with
pb login first. Flags choose the machine name and which incoming services it accepts, and
--ssh-port names the loopback port of the SSH server already running here (default 22). A recovery key is written only where
--recovery-output says; store it carefully.

On Windows, setup prepares OpenSSH first and may ask for administrator approval. On macOS a
hostname such as Name.local is shortened to its first label for the machine name.`,
		Example: `
  pb setup --name studio
  pb setup --name studio --ssh-port 2222 --managed-ssh=false
  pb setup --recovery-output /root/pb-recovery.txt --json`},
	"pb install": {Example: `
  pb install
  pb install --install-dir /usr/local/bin --json`},
	"pb uninstall": {Example: `
  pb uninstall
  pb uninstall --confirmation "UNINSTALL PAPERBOAT" --hostname "$(hostname)"`},
	"pb reset": {Example: `
  pb reset --confirmation "RESET PAPERBOAT" --hostname "$(hostname)" --enrollment-token-file /root/pb-token`},
	"pb doctor": {
		Long: `Check authentication, the local runtime and connectivity for this machine, or for one
machine you name. Without --repair the command only reports findings and how to recover; --repair
performs the supported local fixes (on Windows it includes a privileged step). Exit status is 0
when everything is healthy and 1 otherwise, so scripts should use --json and the exit status.`,
		Example: `
  pb doctor
  pb doctor studio --json
  pb doctor --repair`},
	"pb bugreport": {
		Long: `Collect a bounded, redacted diagnostic bundle for support. The bundle holds a manifest,
recent events, redacted events and status. Nothing leaves this machine unless you pass --upload;
--record controls capture. The command prints where the bundle is and its support reference;
review it before you share it.`,
		Example: `
  pb bugreport
  pb bugreport --upload
  pb bugreport --json`},
	"pb status": {
		Long: `Show this machine's Paperboat state, or the observed status of the machine you name. It is
a snapshot, not a wait: use pb doctor for a deeper check, pb service status for the background
service and pb wait to wait for a condition. On a machine that is waiting to be added, status
reports the signed-out state; run pb login.`,
		Example: `
  pb status
  pb status studio
  pb status --json`},
	"pb wait": {
		Long: `Wait until a machine satisfies a readiness condition chosen with --for. runtime waits for an
eligible runtime that is ready or degraded. transport (the default) also probes an authenticated
connection to the machine. ssh also waits for managed SSH to be ready. --timeout bounds the wait
(up to 24h); mere connectivity is never treated as readiness.

Exit status: 0 ready, 203 timed out, 205 cancelled, 1 any other failure.`,
		Example: `
  pb wait studio
  pb wait studio --for ssh --timeout 10m
  pb wait studio --json`},
	"pb ping": {
		Long: `Send authenticated health exchanges to one machine and print, per sample, the path used
(direct, peer_relay, regional_relay or unknown) and the connect time, then a loss summary.
--count sets the number of samples (1-100, default 4) and --timeout bounds each (default 10s,
at most 1m). Exit status is 1 when every sample is lost. An offline machine is an error, not a
lost sample. A reachable transport does not mean the service on the machine is ready.`,
		Example: `
  pb ping studio
  pb ping studio --count 10 --timeout 5s
  pb ping studio --json`},
	"pb resolve": {Example: `
  pb resolve studio
  pb resolve studio --json`},
	"pb approve": {Example: `
  pb approve 01JABCDEF
  pb approve 01JABCDEF --json`},
}
