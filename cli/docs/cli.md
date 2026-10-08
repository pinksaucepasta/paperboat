# Using the Paperboat CLI

The [complete command reference](cli/README.md) documents every public `pb` command,
including terminal sessions, SSH/SCP/SFTP/rsync, previews, tunnels, authentication,
configuration, environment variables and vaults, teams, sends, hosted edges, diagnostics,
updates, and runtime management. Each page includes usage, local and inherited
flags with defaults, and links to its parent and children. Commands with examples
in their help include those examples in both formats.

## Read help and man pages

```sh
pb --help
pb preview --help
pb tunnel create --help
pb config customize --help
pb tunnel create --help --json
```

The checked-in [man pages](man/man1) use section 1. Spaces in command names become
hyphens: `pb tunnel create` is `pb-tunnel-create(1)`. On systems with a man reader,
from the repository root:

```sh
man -l docs/man/man1/pb.1
man -l docs/man/man1/pb-preview.1
# Install for your own account, without root:
make install-man MANDIR="$HOME/.local/share/man"
man -M "$HOME/.local/share/man" pb
man -M "$HOME/.local/share/man" pb-tunnel-create
```

The normal Linux installer installs bundled manuals automatically beside its binary
prefix: the default is `~/.local/share/man/man1`. The macOS bootstrap extracts its
verified PKG and the public `pb install` command installs manuals for the owner-scoped
canonical service path; it does not run the package installer. A custom Linux
prefix may need `man -M PREFIX/share/man pb` if it is outside the system's manual
search path. The pages come from the verified executable; no additional download
is needed. Re-running the installer repairs and refreshes installed pages.

`make install` also installs man pages under `PREFIX/share/man`. `install-man`
supports `MANDIR` and `DESTDIR` for system packages or staging. Copying a raw binary
by hand does not extract its bundled manuals. Windows users can read the Markdown
reference or use built-in `--help`; a Unix man reader is not required.

Managed `pb update` refreshes manuals from the verified committed executable.
Linux uses the enrolled user's `~/.local/share/man`; macOS uses
`/usr/local/share/man`. Manuals remain unchanged if a candidate rolls back before
commit. If extraction fails after commit, the healthy runtime remains active and
the update stays pending while recovery retries; correct directory permissions or
free disk space, then retry `pb update`. Partial page writes are repaired on retry.
Older macOS updaters need an installer rerun once to adopt the expanded PKG format.

## Interactive use

Run `pb` in a terminal for the home screen. Type to filter, move with arrow keys,
select with Enter, and return with Escape. **All commands** discovers commands
from the same definitions used by this reference. Nested actions retain explicit
`--config` and `--server` settings. Input validation appears beside the field.
Long results can be scrolled with Page Up and Page Down.

`pb preview` without a target opens guided setup. `pb preview 3000` starts a
foreground preview console in a terminal. Its default keys are `o` to open the
URL, `b` for background handoff, `t` for durable tunnel creation, and `s` or Ctrl+C
to stop. Background handoff requires a matching updated daemon and preserves the
URL. Durable creation waits for readiness and creates a new URL. Custom domains
are not automatically moved between those resources. See the [preview reference](cli/pb_preview.md)
and [preview and tunnel guide](preview-tunnels.md).

[Customization](cli/pb_config_customize.md) supports local command shortcuts,
command defaults, themes, keybindings, menu ordering and visibility, favorites,
columns and optional preview panels. Changes are staged until saved. See the
[configuration example](../README.md#make-the-cli-yours) for the JSON schema and
literal argument templates. Account synchronization is deferred.

## Local browser URLs

The default local browser domain is `local.pprbt.dev`; changing it affects
browser URLs only and leaves Paperboat's native machine names unchanged. Set a
custom domain with `pb config local-access domain set example.com`. Before using
one, configure a DNS-only wildcard record `*.example.com` that resolves to
`127.100.0.1`; Paperboat does not run a DNS server or edit resolver/hosts files.
The command applies local certificate trust and helper configuration before
reporting success. These commands apply to the installed user's active config;
check its path with `pb config path`. Applying an alternate `--config` file is
rejected because the installed helper reads the active config. To map an
already authorized machine port to a readable name, run
`pb config local-access alias set studio jellyfin 8989`. With the default
domain, this creates `https://jellyfin.studio.local.pprbt.dev`; with
`example.com`, the URL is `https://jellyfin.studio.example.com`. It does not
grant access to the port, and Paperboat publishes the name only while that port
is authorized. Use `pb config local-access show` to inspect the effective
settings, `alias unset` to remove a mapping, and `apply` after editing the
`local_access` section of the config file directly.

### Coolify and other reverse proxies

App-name routing automatically uses authorized port 80 on each machine.
No Paperboat command is needed for Coolify’s default HTTP proxy.
If your reverse proxy uses another port, configure that override on the browsing computer:

```console
pb config local-access proxy set homelab 8081
```
Set an application's HTTP domain in Coolify to `jellyfin.homelab.local.pprbt.dev`,
then open `https://jellyfin.homelab.local.pprbt.dev` from this computer.
Paperboat handles local HTTPS and forwards the original hostname privately to
Coolify, which selects the application. Keep Coolify's backend HTTP configuration;
it does not need to obtain a public certificate for these local names.
New app names work without another Paperboat setting. No public tunnel or DNS
changes are required for the default local browser domain.

Numeric URLs such as `8080.homelab.local.pprbt.dev` always select port 8080;
explicit service aliases take precedence over the proxy. The machine base URL
keeps its service index. Only one nonnumeric app label is supported; nested app
names are rejected. Proxy settings do not grant machine or port access and are
active only while the machine and proxy port are authorized and available.
Automatic service discovery covers this user’s ports from 1024 upward; port 80
needs explicit private access authorization. An explicitly authorized loopback
service can be a root-owned or Docker-published port; automatic discovery does
not expose other users’ services.
Use `pb config local-access show` to inspect settings or
`pb config local-access proxy unset homelab` to restore the automatic port-80 default while
keeping numeric ports and explicit aliases.

## Scripting and output

Use explicit commands and supply required operands in scripts. Pass `--json`
for structured output and `--no-customization` when local shortcuts or defaults
must not affect automation. Explicit flags override configured defaults. Use
`pb COMMAND --help --json` for machine-readable command discovery.

JSON output does not start a TUI. Finite commands return JSON results; streaming
operations can emit multiple records rather than a single JSON document. Inspect
the command's output contract before piping a stream into a parser. Raw terminal
and native transfer commands reject unsupported JSON output before execution;
`--json` does not encode terminal traffic or transferred file contents.

Ordinary structured errors identify the failure and may include retryability,
whether state changed, uncertain outcomes, and recovery instructions. Never assume
an unsuccessful command means nothing was created. Remote execution can return
the remote process exit status; treat nonzero exits as failures and inspect the
operation's result before retrying. Do not parse human prose as a stable API.

The delimiter `--` separates command flags from literal command payload. For example:

```sh
pb exec Studio --cwd /src -- git status
pb config customize explain -- mac -- uptime
pb --no-customization environments --json
```

Native SSH/transfer arguments follow the respective tool's semantics. Paperboat
shortcuts and the interactive command palette support quoting and escaping but do
not perform shell expansion or execute local shell statements.

## Authentication and recovery

Install and enroll using the dashboard command, or generate both Linux/macOS and
Windows install commands with `pb machine add` on an authenticated machine.
There is no shell selector.

Run `pb login` (alias for `pb auth login`) to sign in through WorkOS and explicitly
approve this CLI. The command prints a short-lived approval link and opens it when
possible. On a headless or remote machine, open that link on another device; there is
no localhost callback listener or token to paste. `--no-browser` prints the same link
without trying to launch a browser. `--json` emits approval and completion states.

If already signed in, choose to keep the current account, authenticate it again, or
sign in with another account. Non-interactive callers choose `--reauth` or
`--change-account` explicitly. The previous login remains usable until replacement
succeeds. `pb switch` aliases `pb auth switch` and continues selecting Personal or a
team for new commands without moving existing resources or machine ownership.

Interrupted login resumes the same approval when you rerun `pb login`. Ctrl+C and
`pb logout` cancel the pending request; cancellation that cannot reach the server is
retained for retry. Dashboard `get.pprbt.dev/install?token=…` bootstrap keeps its direct
installation-token flow, issuing the same account/session credentials as browser approval.

Use `pb auth status` to inspect the current account and `pb doctor` for
diagnostics. `--server` selects the control plane;
use only the intended server and account.

If local preferences are invalid, `pb --no-customization ...` bypasses them.
`pb config customize validate`, `import`, and `reset` provide repair paths.
Reset and other destructive commands print a confirmation command with a short-lived
code; run it to apply the change. A preview exits with status 2 so scripts can distinguish
it from a completed operation.
Reset affects local CLI preferences, not account or connection settings.

Follow the command's recovery instructions after interrupted preview/tunnel or
update operations. See [operational guidance](operations.md), [runbooks](runbooks.md),
[configuration sync](config-sync.md), and [team lifecycle](team-lifecycle.md) for
workflow details and boundaries. Generated documentation describes the command
interface; it is not evidence that a deployment or connected-machine scenario has
been verified.

## Maintain the reference

```sh
make cli-docs        # regenerate all Markdown and man pages
make cli-docs-check  # fail on missing, changed or obsolete pages
```

The generator traverses the production Cobra command tree without executing any
command, loading user preferences or contacting services. Public completion commands
are included; hidden/internal commands are excluded. Update command descriptions,
examples and flag help in `cmd/pb`, then regenerate. The generation date is fixed
for reproducible output. No separate hand-maintained command inventory is used.

## Updates

`pb update check` checks signed release metadata without changing the installation.
`pb update download` downloads and verifies an available release without running it.
Review the reported version, platform, size and SHA-256 digest, then install with
`pb update --approve <candidate-id>`. Interactive `pb update` presents the same
download for confirmation; JSON and noninteractive use only download unless an
exact candidate ID is supplied.

Official installations have scheduled updates enabled by default. Source and custom
installations default to availability checks only. Scheduled updates check for a
release and download and verify it ahead of the maintenance window, then install it
at 04:00 in the machine's local time zone. Configure or inspect this machine-level
schedule with `pb update settings`:

```sh
pb update settings                 # show the current schedule
pb update settings --time 22:15    # choose a local maintenance time
pb update settings --auto=false    # disable scheduled downloads and installs
pb update settings --auto=true     # enable them again
```

With automatic updates off, availability checks continue while downloads and
installation wait for manual action. The manual `pb update check`, `download`, and
`--approve` flows remain available; `pb update download` stages a verified release
without installing it.

Ordinary updates replace the feature worker while the process owner keeps shells
and commands alive. Connections briefly interrupt and reconnect. A rare update
that replaces the process owner announces a bounded deadline; running terminals
and commands end at that restart. Manual approval warns about this interruption.
`pb update settings --auto=false` cancels automatic installation while it is still
pending. A terminal client retries an unexpected transport loss up to its configured retry
limit (six attempts by default), with delays backing off to at most 30 seconds. It
recovers available terminal output after reconnecting. Check `pb update status`
for completion, pending maintenance, or an actionable recovery error.

Administrators can set a minimum supported version and an enforcement date.
Official releases warn ahead of that date and require an update on the next
feature command once it takes effect. Local and custom builds are exempt.
Login, update, diagnostics, and cleanup remain available for recovery.
