# paperboat

The Paperboat command-line client. `pb` authenticates the user, selects an environment,
attaches to helper-managed terminal sessions, and bridges local file pastes into remote
agent workflows.

The CLI uses Paperboat device sessions and stores secrets in the operating-system
credential store. It does not own remote PTYs, tunnel infrastructure, or reusable
connector credentials.

Control-plane requests identify the CLI and protocol version so incompatible clients receive
an actionable upgrade error instead of malformed session data. See
[docs/operations.md](docs/operations.md) for security and outage handling.

## Usage

```sh
pb <environment>             # attach a hosted project or machine terminal
pb environments               # list hosted projects and machines
pb auth login                # approve this installation in the dashboard
pb auth status               # show the active account for the configured server
pb auth switch               # replace the active account for this server
pb auth logout               # revoke and remove this installation's session
pb doctor                    # check auth + environment connectivity
pb Studio -- git status      # execute an exact argv vector on a machine
pb exec Studio --cwd /src -- make test
pb config path|show          # inspect the local config
pb preview 3000              # publish a local port
pb preview ./dist            # publish a local file or directory
pb preview 3000 --private    # require the local Paperboat runtime
```

Flags may appear before or after the environment name.
Hosted projects and machines use the same durable terminal-session workflow:
`--new`, `--session`, and `pb sessions` apply to either environment type.

## Remote execution

`pb <machine> -- <argv...>` executes an exact argument vector without an implicit shell.
Use `pb exec` for execution controls such as `--cwd`, `--timeout`, repeated `--env
name=value`, `--pty`, and `--json`. Non-PTY execution keeps stdout and stderr separate;
PTY mode merges them and forwards terminal resize events. JSON mode emits the versioned
`paperboat.exec-event/v1` JSON Lines stream and returns the remote process exit status.

## Preview a local target

`pb preview <port|url|path>` exposes one local HTTP service, file, or directory through a
temporary Paperboat preview. The stable host runtime owns the authenticated carrier and
keeps the server lease, route, and origin readiness synchronized.

```sh
pb preview 3000
pb preview http://127.0.0.1:8080
pb preview ./report.html --ttl 1h
pb tunnel --ephemeral 3000
pb preview 3000 --background --ttl 1h
pb preview ./dist --domain preview.example.com
pb preview 3000 --private
```

Use `pb preview list` or `pb preview status <preview>` to inspect ephemeral tunnels;
`stop` and `delete` both withdraw one. Background ownership defaults to 30 minutes, is
capped at 24 hours, and is not restored after reboot. `--domain` attaches a verified
custom domain and may be repeated. Private preview
hostnames are routed through the narrow local Paperboat proxy; browsers receive no
Paperboat credential, cookie, or redirect flow.

Interactive attaches forward `TERM`, `COLORTERM`, `TERM_PROGRAM`,
`TERM_PROGRAM_VERSION`, and locale variables when they are set locally.

## Interactive status bar

Compatible interactive terminals reserve one bottom row for local Paperboat context. The
remote PTY receives the remaining rows, and no status-bar bytes are sent remotely. By
default the bar inherits the terminal palette and temporarily releases its row when a
remote application enters the alternate screen, preventing editors and other full-screen
TUIs from losing or covering content.

```sh
pb config status-bar show
pb config status-bar preview
pb config status-bar set mode auto                 # auto, on, off
pb config status-bar set fullscreen hide           # hide, show
pb config status-bar set theme terminal             # terminal, dark, light, mono
pb config status-bar set privacy true
pb config status-bar set terminal-title true
pb config status-bar set left project,session
pb config status-bar set center activity
pb config status-bar set right credits,connection
pb config status-bar set accent '#00d7af'
pb config status-bar reset
```

Use `none` to empty a widget region. Supported widgets are `project`, `session`,
`connection`, `activity`, `config_sync`, `credits`, and `storage`; a widget can appear in
only one region. Color overrides accept ANSI names such as `bright_cyan`, `default`, or
`#RRGGBB`. `NO_COLOR` disables status-bar colors without disabling the bar.

Attach flags override saved behavior for one session:

```sh
pb demo --status-bar=off
pb demo --status-bar-fullscreen=show --status-bar-theme=mono
pb demo --path=d
```

Terminal attachments use `connect.terminal_transport`, with `a` (the default), `d`, `q`,
`w`, or `r`. Auto races direct and relay paths, prefers direct, and keeps a relay standby
while the application is active. `--path` overrides the mode for one command without
rewriting configuration. Explicit path modes never select a path outside their contract.

The bar automatically drops storage, credits, config-sync, session, and project widgets
in that order as width becomes constrained. Connection and active failure state retain
priority. Terminals narrower than 20 columns receive the full viewport without a bar.

## File drag and drop

Interactive `pb` attaches enable the standard terminal bracketed-paste mode. A
framed paste containing only absolute local file paths is staged and rewritten
to remote paths. Supported path forms include `file://` URIs, quoted paths, and
POSIX shell-escaped paths such as the escaped-space format emitted by WezTerm.
Every other input, including unframed drag-and-drop text, is forwarded exactly
as received; this prevents ordinary typed paths from triggering an upload.

Files are hashed once through the validated local descriptor, rewound, and streamed through
the resumable `/v1/file-transfers` API. Mixed and empty files are supported without MIME
restrictions. A paste is rewritten only after the whole batch publishes atomically; retries
reuse transfer IDs and confirmed offsets, and failures preserve the exact original paste.
Published remote files remain for seven days.

Use `pb send <path>... --to <machine>` to deliver files to another machine's configured
Paperboat Inbox. Session and user defaults are explicit, multi-attachment ambiguity never
selects the latest writer, and the sender exits successfully only after the destination
verifies size and SHA-256, fsyncs the file, avoids name collisions, and records a durable
receipt. Inbox files remain until the user removes them.

## Machines

Run `pb setup` to register this device, create its Paperboat Inbox, verify the
server-selected TUF target, and install the Paperboat service. A device can initiate
authorized operations and can receive the incoming services enabled in its capability
settings. Terminal, managed SSH, native file receiving, and preview/tunnel serving are
enabled by default; account peer relay is opt-in.

`pb pair` performs the same unified-device enrollment when invoked by a dashboard-issued
one-shot install command. It does not select a permanent host or client role.

When no observability path is configured, metadata-only events are appended to
`telemetry.jsonl` beside the CLI config with mode `0600`. Set
`observability.disable_event_log` to `true` to opt out. The log is bounded and
truncated at `observability.max_event_log_bytes` rather than growing without limit.

## Build

Install Go and `ripgrep` before running development checks. The Makefile selects the
pinned Go toolchain and runs the pinned `sqlc` generator. Run commands from this
repository's root:

```sh
make build      # -> bin/pb
make check      # formatting, generated code, vet, tests, and build
make release-assets   # the five native release assets in dist/
make install    # install pb
make test       # Go test suite
```

Release packaging additionally needs the platform signing and publishing credentials.
It is not required for a local development build.

`YYYY.MM.DD.X` tags build and publish the five native assets after their platform checks and
GitHub API digest verification. Use `tools/release-version.sh next` to generate the next tag;
tags have no `v` prefix.

Releases contain one complete `pb` asset per supported platform and architecture:
Windows amd64/arm64 PE executables, Linux amd64/arm64 raw ELF executables, and one signed
and notarized macOS arm64 installer package. Users install through
[`https://get.pprbt.dev/install`](https://get.pprbt.dev/install); the installer verifies
`current.json`, downloads the selected bytes from the immutable GitHub release URL, and
checks the declared length and SHA-256 before installation.

## Stack

Go - distributed as native static binaries for Windows, macOS, and Linux (Cobra, Go 1.27.1).
Windows amd64 and arm64 are stable after native release qualification.

## Layout

- `cmd/pb` — CLI entrypoint (commands, flags, wiring).
- `internal/config` — local policy and secure, versioned credential profiles.
- `internal/resolver` — paginated environment resolution and validated connect descriptors.
- `internal/tunnel` — native QUIC-first and WSS terminal-v1 transports with bounded reconnect supervision.
- `internal/session` — transparent PTY wrapper (raw mode, resize, exit-code passthrough).
- `internal/paste` — bracketed-paste interceptor and atomic file-path batch rewriter.
- `internal/filetransfer` — resumable HTTP/3-first, HTTP/2-fallback file transport.
- `internal/inbox` — durable Paperboat Inbox download and receipt handling.

## License

MIT. See [LICENSE](LICENSE).

## Team machines

Each OS user enrolls their own machine identity and keeps their own local PB credentials.
Teammates use their own accounts and devices to connect; do not copy private keys or add
another target-side enrollment for each teammate. Work on one enrollment runs as its OS
user. Separate terminals are the default, but OS files and processes remain accessible
under that same OS user's permissions. Machine grants are not OS isolation and do not
allow attaching to another person's terminal.

A personal owner can share and withdraw their machine. Sharing creates no use grants.
Read the current team generation with `pb team get` before each mutation:

```sh
pb team get research
pb team machine share research MACHINE_ID --generation CURRENT_GENERATION
pb team machine grant research MACHINE_ID --member ACCOUNT_ID \
  --capability terminal,exec,files --generation CURRENT_GENERATION
pb team machine grant research MACHINE_ID --all-members \
  --capability managed_ssh --generation CURRENT_GENERATION
pb team machine unshare research MACHINE_ID --generation CURRENT_GENERATION
```

The six exact capabilities are `terminal`, `exec`, `managed_ssh` (including SCP, SFTP and
rsync), `files`, `preview_manage`, and `tunnel_manage`. All-member grants apply to current
and future accepted members; selected-member grants apply to one accepted account.
Their capabilities combine. Saving a grant replaces that audience's capability list;
`--active=false` revokes it. Device incoming toggles must also allow the requested operation.
Tunnel management covers existing private/team tunnels: routes, desired state, deletion, and connector status and controls. Create tunnels locally on the target machine; a teammate’s grant does not copy its enrollment proof or launch a remote connector. Public publication, domains, and connector enrollment require separate owner authority.

No machine grant implies ENV administration, public publication or resharing.

A personal owner who is also a team owner/admin can explicitly transfer the enrollment:

```sh
pb team machine transfer-to-team research MACHINE_ID \
  --generation CURRENT_GENERATION --confirm MACHINE_ID
```

The team then controls the enrollment, other personal team shares are withdrawn, and the
enroller loses implicit personal control. Remaining authorized members can keep using it
when the enroller leaves. Team owner/admin roles govern management; machine use still
requires a grant. Transferring the team to a new owner preserves its machine ownership.
`pb team machine remove` requires the same exact confirmation and revokes the team-owned
enrollment. Deleting the team also revokes its team-owned enrollments, without converting
them to personal property. Personal machines remain personal; withdrawing a share or
leaving the team removes that team's access. Use the dashboard's Teams page for the same
ownership and grant controls; Machines shows current ownership and your permissions.

## Shared terminals

Terminal sessions are private by default. Machine access or team membership does not
allow joining someone else's session. The session owner can explicitly grant access:

```sh
pb session shared
pb session share SESSION_ID --team TEAM_ID --member ACCOUNT_ID --role viewer
pb session share SESSION_ID --team TEAM_ID --all --role interactive
pb session join SESSION_ID
pb session participants SESSION_ID
pb session remove SESSION_ID ACCOUNT_ID
pb session unshare SESSION_ID
```

The dashboard's **Terminals** page provides the same sharing and participant controls.
Only the session owner administers sharing; a team administrator does not automatically
control personal sessions. A selected member's role overrides the all-team default.
Removing a participant also excludes them from all-team access until explicitly regranted.

Viewers receive output but cannot type, resize or signal the shell. Ctrl-C detaches the
local viewer. Interactive participants use the shell's OS permissions: their input is
serialized in host receive order, but simultaneous typing may interleave. They can send
signals and commands that end the shell. Paperboat identities do not isolate processes
or files shared by the same OS user.

Joining includes the existing recent output, at most 64 KiB and further bounded by
attachment capacity, followed by live output. That output may contain secrets regardless
of ENV permissions. It is neither unlimited history nor an exact screen snapshot.
Reconnect checks current access and resumes within the retained output; a gap is reported
when the earlier output is no longer retained. Shared joins cannot create or restart the
owner's shell. Disconnecting a participant leaves the shell and other participants alone.
Ending sharing withdraws teammate access while preserving the owner's session.
Sharing is tied to the current shell process. Restarting the shell detaches shared
participants, and old grants cannot read or control its replacement. The owner must
explicitly share the new shell. Closed or exited sharing is retired when the control
plane observes the runtime; a new shared connection checks the runtime first.
New connection credentials always require a current grant. Active connections close when
the runtime receives revocation or their credential expires. Revocation refresh uses
a 15-second interval and a 10-second request timeout; shared credentials last
at most five minutes, bounding access if the control plane cannot be reached.
