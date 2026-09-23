# Using the Paperboat CLI

The [complete command reference](cli/README.md) documents every public `pb` command,
including terminal sessions, SSH/SCP/SFTP/rsync, previews, tunnels, authentication,
configuration, environment variables and vaults, teams, transfers, diagnostics,
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

For CLI authentication on an existing installation, run `pb auth login` and
paste the same 26-character enrollment token into the hidden prompt. This creates
a CLI session without opening a browser, redirecting to the dashboard, installing
services, or creating a machine. Tokens are single-use: use a fresh token for a
separate installation. For scripts, use `pb auth login --token-file /absolute/path
--json` with a protected token file; the caller owns removal of that file.

Interrupted login resumes using protected local recovery state when you rerun
`pb auth login`. Invalid, expired, cancelled, or already-used tokens require a
fresh token. `pb auth logout` cancels pending login and removes local sessions;
if cancellation cannot reach the server, retry logout when connectivity returns.

`pb login` and `pb auth switch` print enrollment guidance and the dashboard URL
supplied by the configured server. Use `pb auth status` to inspect the current
account and `pb doctor` for diagnostics. `--server` selects the control plane;
use only the intended server and account.

If local preferences are invalid, `pb --no-customization ...` bypasses them.
`pb config customize validate`, `import`, and `reset --yes` provide repair paths.
Reset affects local CLI preferences, not account or connection settings.

Follow the command's recovery instructions after interrupted preview/tunnel or
update operations. See [operational guidance](operations.md), [runbooks](runbooks.md),
[configuration sync](config-sync.md), and [team lifecycle](team-lifecycle.md) for
workflow details and boundaries. Generated documentation describes the command
interface; it is not evidence that a deployment or connected-device scenario has
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
