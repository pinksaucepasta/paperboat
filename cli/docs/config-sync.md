# Configuration sync

All targets are machines/machines. Configuration sync has one authoritative TOML file
per machine and one optional shared file in the repository. Run `pb config sync path`
to find the machine file. Shared defaults belong in `.paperboat/config-sync.toml`.
Both use the v1 format; an empty or comment-only file is valid. No additional include/ignore files
or server-side rule editor are required.

## Setup

Connect a GitHub or custom Git repository, then run on the machine being configured:

```text
pb config sync init
pb config sync path
pb config sync validate --repository <repository-id> --json
pb config sync apply --repository <repository-id> --json
```

`init` creates an empty machine file without overwriting an existing one. Edit that file
or commit shared defaults in the repository, then validate. `apply` previews the effective
configuration and supplies a confirmation command; repeat it with the displayed
`--confirm` code to approve the exact configuration and current assignment version.
The interactive Config sync menu offers repository setup, file instructions, applying
configuration, review, status, worker repair, and disabling. Files are configured for
the machine running the CLI; a remote terminal configures that remote machine.

Credentials, `.env`, SSH material and Paperboat runtime state are always excluded.
Selected files and their Git history are ordinary plaintext at the repository owner.
Keep secrets in ENV rather than tracked files. Transport encryption does not encrypt
Git history at rest. Destinations require normal OS write permission; sync does not
elevate to write protected paths.

## Shared defaults, OS settings and machine overrides

The shared file can define common settings and explicit OS-specific rules. For example:

```toml
version = 1
mode = "pull_only"
automatic_updates = false

[os.linux.paths."myapp/settings.json"]
local_path = "~/.config/myapp/settings.json"
kind = "file"

[os.darwin.paths."myapp/settings.json"]
local_path = "~/Library/Application Support/myapp/settings.json"
kind = "file"

[os.windows.paths."myapp/settings.json"]
local_path = "%APPDATA%/myapp/settings.json"
kind = "file"
```

These paths are chosen by the user; Paperboat never translates or infers destinations.
Precedence is **machine file → matching OS section → shared defaults**. An empty machine
file inherits applicable defaults. To change only one machine setting, add that setting
locally; all other defaults remain inherited. The machine file cannot contain OS sections.
OS keys are `linux`, `darwin`, and `windows`.

Rules are keyed by repository-relative file or directory path. For the same path, an
explicit field overrides the inherited field; omitted fields stay inherited, and empty
include/exclude arrays clear inherited patterns. More specific higher-priority rules
own their file/subtree before selection patterns are evaluated, so excluding an override
never falls back to a broader rule. Ambiguous rules at the same priority are rejected.
Empty effective rules synchronize no files.

A machine override can select direction, repositories and only the files it needs:

```toml
mode = "push_only"

[push]
repository_id = "cfgrepo_example"

[paths."myapp/settings.json"]
local_path = "~/.config/myapp/settings.json"
kind = "file"

[paths."myapp/keybindings.json"]
local_path = "~/.config/myapp/keybindings.json"
kind = "file"
```

Directory rules preserve relative suffixes. `include` and `exclude` arrays use relative
`*`, `?`, `[]`, and recursive `**` glob patterns. Empty include selects all eligible
files under the directory; exclusions win. `~`, `$XDG_CONFIG_HOME`, `%APPDATA%`,
`%LOCALAPPDATA%`, and `%USERPROFILE%` expand only when explicitly written and available
in that machine's user context. Missing variables fail rather than choosing substitutes.
Unsafe paths, destination collisions, case-colliding names and symlink/reparse escapes
are rejected. Different file formats need explicitly selected repository variants;
path mapping does not convert content or execute scripts/templates.

Machine or shared configuration changes pause synchronization until `pb config sync apply`
approves the new effective configuration. Automatic updates authorize later file changes
within approved rules, never new destinations or direction changes. Removing a rule
relinquishes management without deleting its old local files. Repository deletions affect
only currently selected files with an established Paperboat baseline.

## Review and recovery

Pull and push repositories can differ. Access, assignment consent, writer leases and
revocation remain enforced. Missing credentials or an unavailable mount/repository pause
sync; they never become empty data or imply deletion. Paperboat never auto force-pushes.
With automatic updates off, review reported `review_required` changes and run
`pb config approve <machine>` to approve that exact repository head. A changed head
makes that approval stale. `pb config unassign <machine>` disables sync while preserving
repository content and applied files.

Paperboat embeds Git operations and three-way text merging: system Git and Git user
name/email are unnecessary. Commits use `Paperboat <config@paperboat.invalid>`.
Independent edits merge automatically; overlapping, ambiguous, or work-budget-exceeding
edits remain conflicts with original versions preserved. File-size limits still apply.
A private recovery journal restores interrupted writes before proceeding. Unrelated
managed files can continue while a conflicting file remains unchanged. Arbitrary local
files do not become visible atomically to other applications.

Team defaults select bootstrap repository metadata, not approved machine paths. A member
uses their own provider access to adopt a team default, then applies the machine/shared
files before sync can activate. Personal repository choices take precedence.

## Custom Git repositories

```text
pb config repository connect ssh://git@host/config.git --name configs --branch main --json
pb config repository connect https://host/config.git --name configs --json
pb config repository connect /absolute/config.git --name local-configs --json
pb config repository list --json
pb config repository credentials set <repository-id> --file /absolute/private-profile.json --json
pb config repository credentials remove <repository-id> --json
```

A custom repository needs no GitHub connection. SSH requires a server providing Git
upload/receive service, although the Paperboat client needs no system Git. Local writes
require a bare repository; a missing mount or unavailable repository pauses sync rather
than treating it as deletion. Other machines reach an owner through SSH/HTTPS or an
explicitly mounted path: being on the same LAN does not itself expose a local repository.

A machine can select an explicit endpoint alias for a registered custom repository in
its configuration file. For example:

```toml
mode = "pull_only"

[pull]
repository_id = "cfgrepo_example"
url = "/mnt/shared/config.git"
```

Aliases must refer to the same actual repository and branch; Paperboat's assignment,
writer lease, consent, and revocation still apply. GitHub endpoints cannot be redirected
through bindings. Do not embed passwords, tokens or private keys in URLs or configuration
metadata. Machine profiles are private files stored outside repository/assignment data.
`--file -` reads a profile from stdin; regular input files must be owner-only.

Example HTTPS token profile:

```json
{"transport":"https","auth":"token","username":"your-provider-user","password":"your-token"}
```

Example SSH key profile (all referenced paths are native absolute machine paths):

```json
{"transport":"ssh","auth":"ssh","ssh_key_file":"/home/user/.ssh/config-sync","known_hosts_file":"/home/user/.ssh/known_hosts"}
```

SSH agent profiles use `ssh_agent:true` instead of `ssh_key_file`; the agent must be
available to the worker process. HTTPS profiles may set `ca_file` for a private CA.
TLS and SSH host keys are verified. HTTP requires an explicit profile with
`allow_insecure_http:true` because it transports plaintext without TLS. Redirects are
rejected, so credentials cannot follow a different endpoint. Authentication failures
leave files and the last known installation intact; repair the profile or endpoint
and retry. Git history is still plaintext at rest, irrespective of transport.
