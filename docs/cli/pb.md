## pb

Open Paperboat or connect to an environment terminal

### Synopsis

Paperboat provides remote terminals, managed SSH and file transfers, previews,
tunnels, environment configuration, team management, and local runtime controls.

Run pb without arguments in a terminal to open the interactive home screen.
Use explicit subcommands in scripts; pb COMMAND --help describes each operation.
Use pb COMMAND --help --json for machine-readable command discovery.

The --json flag selects machine-readable output and suppresses interactive menus.
Raw terminal and native transfer commands reject unsupported JSON output before
execution. Streaming commands may emit multiple JSON records; consult their help.

Local preferences can define shortcuts, command defaults and TUI appearance.
Run pb config customize to edit them, or use --no-customization to bypass them.
Explicit flags override saved defaults. Built-in command names remain reserved.
Numeric invocations such as pb 3000 select the configured preview or tunnel action.

Run pb doctor for diagnostics. Check command output for partial changes and
recovery instructions before retrying an operation that may have created resources.

```
pb [environment] [new] [flags]
```

### Examples

```
  pb
  pb login
  pb environments
  pb connect Studio
  pb ssh Studio
  pb preview 3000
  pb tunnel create demo --port 3000
  pb config customize
  pb --no-customization environments --json
```

### Options

```
      --config string                  path to the CLI config file
      --debug                          show the pb versions used by this terminal session
  -h, --help                           help for pb
      --json                           print machine-readable JSON
      --name string                    name for the fresh terminal session
      --no-customization               ignore local shortcuts, command defaults, and TUI preferences
      --server string                  paperboat-server base URL override
      --session string                 attach an existing terminal session by name or ID
      --status-bar string              status bar for this attach: auto, on, or off
      --status-bar-fullscreen string   status bar in full-screen applications: hide or show
      --status-bar-theme string        status bar theme: terminal, dark, light, or mono
      --transport string               peer transport: a (auto), d (direct QUIC), q (relay QUIC), w (relay WSS), or r (relay race)
  -v, --version                        version for pb
```

### SEE ALSO

* [pb access](pb_access.md)	 - Open authenticated private access
* [pb approve](pb_approve.md)	 - Approve or revoke a peer device over gRPC IPC
* [pb auth](pb_auth.md)	 - Manage Paperboat sign-in
* [pb bugreport](pb_bugreport.md)	 - Create a redacted Paperboat diagnostic bundle
* [pb completion](pb_completion.md)	 - Generate the autocompletion script for the specified shell
* [pb config](pb_config.md)	 - Inspect the local CLI config
* [pb connect](pb_connect.md)	 - Create and attach to an environment terminal session
* [pb daemon](pb_daemon.md)	 - Paperboat endpoint daemon
* [pb desktop](pb_desktop.md)	 - Authenticated desktop management bridge
* [pb doctor](pb_doctor.md)	 - Check Paperboat connectivity and readiness
* [pb env](pb_env.md)	 - Manage ENV Injection for connected hosts
* [pb environments](pb_environments.md)	 - List machines available to this account
* [pb exec](pb_exec.md)	 - Execute an exact command on a machine
* [pb inbox](pb_inbox.md)	 - Manage the Paperboat Inbox
* [pb install](pb_install.md)	 - Install this executable and its local service
* [pb login](pb_login.md)	 - Show dashboard enrollment instructions
* [pb logout](pb_logout.md)	 - Revoke and remove the active client session
* [pb machine](pb_machine.md)	 - Manage machines
* [pb pair](pb_pair.md)	 - Enroll this device with a one-shot token
* [pb ping](pb_ping.md)	 - Measure authenticated connectivity to a machine
* [pb preview](pb_preview.md)	 - Expose a local target through a temporary preview
* [pb relay](pb_relay.md)	 - Inspect Paperboat relays
* [pb reset](pb_reset.md)	 - Remove the current Paperboat setup before fresh enrollment
* [pb resolve](pb_resolve.md)	 - Resolve a peer device IP, port forwardings and tags over gRPC IPC
* [pb rsync](pb_rsync.md)	 - Run rsync with Paperboat machine resolution
* [pb scp](pb_scp.md)	 - Run scp with Paperboat machine resolution
* [pb send](pb_send.md)	 - Send files to a machine's Paperboat Inbox
* [pb service](pb_service.md)	 - Manage the Paperboat background daemon service
* [pb session](pb_session.md)	 - Manage environment terminal sessions
* [pb sessions](pb_sessions.md)	 - 
* [pb setup](pb_setup.md)	 - Set up this machine for Paperboat
* [pb sftp](pb_sftp.md)	 - Run sftp with Paperboat machine resolution
* [pb ssh](pb_ssh.md)	 - Connect to a machine with OpenSSH
* [pb status](pb_status.md)	 - Show local Paperboat machine status
* [pb tag](pb_tag.md)	 - Assign tags to a device over gRPC IPC
* [pb team](pb_team.md)	 - Manage teams and explicit resource permissions
* [pb transfer](pb_transfer.md)	 - Manage file transfers
* [pb tunnel](pb_tunnel.md)	 - Manage durable tunnels or start an ephemeral tunnel
* [pb uninstall](pb_uninstall.md)	 - Completely remove Paperboat from this machine
* [pb update](pb_update.md)	 - Update pb from the signed Paperboat release
* [pb wait](pb_wait.md)	 - Wait for a machine readiness condition

