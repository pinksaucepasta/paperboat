# Configuration Sync Contract

The control plane is authoritative for repository connections, machine assignment,
plaintext-storage consent, machine eligibility, scoped repository access, writer leases,
conflict actions, and status revisions. The assigned `pb` config worker alone reads or
mutates managed home paths and Git worktrees.

## Rollout and compatibility

- `disabled`: no helper config runtime or repository access.
- `read_only`: current eligible machines may restore, poll, report, and preserve
  conflicts. Writer lease acquisition returns `config_writes_disabled`.
- `leased_writes`: current eligible helpers may acquire fenced writer leases.
- Every machine requires the current plaintext-storage warning
  consent before credentials, repository access, or synchronization can proceed.
- Missing or unknown runtime mode, policy format, policy revision, machine generation,
  assignment, warning revision, or mandatory exclusions fails disabled.

## Repository and assignment

GitHub repository identity is `(owner_user_id, provider, external_repository_id)`. Provider
owner/name and clone URL are display/transport data, never identity. A repository is
usable only in `active` state with current authorization and scoped-access capability.

Custom `provider=git` registrations accept HTTPS, HTTP, SSH (`ssh://` or scp-like),
and explicit absolute local/file URLs, with a selected branch (default `main`).
Their external repository identity is an opaque control-plane value; endpoint aliases
are transport coordinates, never new writer-lease identities. The server does not probe
private endpoints or collect custom credentials. Reconnecting the same owner/endpoint
preserves the registered identity. An active custom connection cannot change its branch;
disconnect first to revoke its assignments and grants before selecting a new branch. Embedded URL passwords, HTTP userinfo, queries,
fragments and traversals are rejected. SSH usernames identify remote accounts.

Custom grants use the distinct `machine_repository` registration capability and return
`transport` (`https`, `http`, `ssh`, `local`), exact authorized endpoint, branch,
read/write capability and expiry with empty username/password. Machines resolve their
credential profile by authoritative repository ID. GitHub access continues to return
HTTPS and its existing x-access-token credential. TLS certificate and SSH host-key trust
must remain enabled. HTTP requires explicit insecure opt-in in the endpoint profile;
registration alone grants no exception and the server sends no custom secrets.

Each machine has at most one assignment. Mutations use `expected_version`.
Disconnect, unassign, consent removal, warning change, machine replacement, machine
revocation, and authorization loss stop new credentials/access/leases before cleanup.

Assignment `path_rules` is an explicit array of `{id, source, repository_path, local_path,
kind, include, exclude}`. Repository paths are ordinary repository-relative paths;
`.` is allowed for a directory root. `local_path` is an explicitly supplied absolute
native path or user-written `~`, `$XDG_CONFIG_HOME`, `%APPDATA%`, `%LOCALAPPDATA%`,
`%USERPROFILE%` expansion prefix on that machine. Tokens may stand alone only for
directory rules or be followed by `/` or `\`; the server stores them unchanged.
The owning worker requires an actual absolute token value from its user context, with
no guessed fallback, same-home or cross-platform destination inference.
File rules map one exact file and have no patterns. Directory rules retain each selected
file's relative suffix. Include/exclude selectors use `*`, `?`, `[]` and `**` relative
to `repository_path`; exclusions win. Empty effective rules are a healthy no-op; an empty machine file inherits shared defaults. Removing or
changing rules relinquishes management without deleting prior or unselected local files.
The worker validates native root access, collisions and filesystem escapes before apply.
Limits are 256 rules, 128-byte rule IDs, 4096-byte paths/selectors, 64 total selectors per
rule and 64KiB for combined rule/binding JSON.

`repository_bindings` contains at most two `{repository_id, url}` entries, each for an
assigned custom Git pull/push target. Bindings authorize that exact machine endpoint;
GitHub bindings are rejected so provider credentials cannot move to another host.
Canonical repository IDs still own writer leases across aliases. Changing rules or
bindings increments assignment version, creates a new assignment identity, clears prior
approval/consent and revokes old credentials, access operations and leases. Runtime
`assignment_version`, rules and bindings let the worker fence baseline and crash-journal
work. Each approved rule also carries `source` (`shared`, `os`, or `machine`), with explicit
priority machine > matching OS > shared. Cross-source repository ancestry is permitted;
same-source ancestry and destination aliases are rejected. Cross-source nested
destinations are allowed only when the descendant repository suffix maps exactly
to the same relative suffix under the ancestor directory destination. Priority is
chosen before selectors; excluded higher-priority ownership never falls back.
The owning machine merges the full file configuration and submits a lowercase SHA-256
`configuration_revision` through the authenticated projection PUT. The server stores
that approval fence, includes it in assignment/runtime DTOs, and never reconstructs it
from incomplete metadata. Digest changes create a new assignment identity and revoke
prior approvals and grants. Pending team/convenience assignments without a projection
revision are bootstrap metadata only: they cannot accept consent or become eligible.
Migration revokes existing unfenced projections; users must explicitly apply their file
configuration and accept current consent again.

Both user-edited source files use TOML. Shared `.paperboat/config-sync.toml`
defaults and the machine configuration file reported by `pb config sync path` own
repositories, operating mode, update preferences, and path rules. Machine fields override
matching OS fields, which override shared defaults. Empty or comment-only TOML machine
files inherit; no server form reconstructs or edits these source files. Repository eligibility exclusions
and mandatory secret exclusions can only narrow the effective selection; they never
supply a destination. Configuration content remains ordinary Git plaintext.
Quoted TOML path table keys preserve repository slashes and dots, for example
`[os.windows.paths."myapp/settings.json"]`. TOML literal strings preserve Windows
backslashes, for example `local_path = '%APPDATA%\MyApp\settings.json'`.
The user files have one TOML reader; assignment and runtime HTTP bodies remain JSON.

## Writer lease

One repository has at most one current writer. A lease binds repository, assignment,
machine, machine generation, base remote revision, expiry, operation ID, and a
monotonic fencing token. Acquire/renew/release are idempotent. A helper:

1. fetches and observes read-only state without a lease;
2. revalidates current authorization immediately before apply;
3. for a publication, acquires against the observed head, fetches and reconciles again;
4. renews and revalidates authorization/head immediately before a normal fast-forward
   push;
5. never retries an ambiguous push until remote observation proves its outcome.

## Conflict

Regular UTF-8 text without NUL bytes is merged in the assigned worker using the common
base and exact local/remote content. Independent line edits compose; overlapping
regions merge only when both sides produce identical content. Competing insertions at
the same position and insertions touching another edit are conservatively conflicts.
Line endings and the absence of a final newline are preserved. No system Git executable
or configured Git user name/email is required; sync commits use the explicit Paperboat
identity.

Input and merged output obey the assigned maximum file bytes. Automatic merging has a
shared 1,048,576-unit line/trace work budget, including both diffs and reconstruction,
to bound CPU and scratch allocation for ordinary config workloads. Work exhaustion,
binary/non-text content and unresolved overlaps retain the existing conflict behavior;
no partial result is applied. Cancellation is checked synchronously between line scans,
frontier steps and reconstruction, without subprocesses, temporary merge files or
background diff goroutines.

An unresolved same-path three-way change stops automated work for that path while
unrelated clean paths continue. Conflict revision is
SHA-256 over repository, assignment, remote head, relative path, and base/local/remote
content digests. Both exact sides are retained only in private, permission-protected local
runtime state; server metadata contains only bindings, relative path, byte count, reason,
revisions, and digests.

Resolution is one of `keep_local`, `keep_remote`, or `externally_resolved`, bound to the
current assignment version, remote revision, path, and conflict revision. Stale actions
are rejected. Baseline advancement, landed-resolution acknowledgement, and artifact
cleanup are crash-journaled.

## States and errors

Canonical states are `disabled`, `consent_required`, `restoring`, `watching`, `pending`,
`syncing`, `healthy`, `warning`, `conflict`, `offline`, `revoked`, `sync_uncertain`, and
`error`.

Stable recovery errors include `assignment_required`, `consent_required`,
`warning_revision_stale`, `credential_expired`, `config_writes_disabled`, `lease_busy`,
`lease_lost`, `remote_revision_changed`, `config_conflict`,
`repository_unavailable`, `flush_timeout`, and `sync_uncertain`.

Status is monotonic by sync revision and bound to repository, assignment, environment,
machine generation, warning, and policy revisions. The runtime descriptor includes the
authoritative sync-revision floor so a restarted worker advances beyond the last
accepted revision. A newer status report for the same revision may replace an earlier
status report; an older worker timestamp or revision is rejected. Summaries are bounded
and never contain file contents, absolute paths, credentials, keys, provider responses,
or command output.

Native account bootstrap uses `POST /v1/config-repositories/{repository_id}/read-access`
with optional `{ "url": "credential-free-machine-alias" }` for custom Git only and
`machines:read` bearer scope. It requires exact active repository ownership, checks
repository version again after issuance, and grants only `repository_contents_read`.
It does not authorize machine filesystem access, publish, or bypass mapped-root consent.
The response is never cached; client access expires within five minutes (provider
credential expiration remains provider-authoritative). GitHub returns its scoped read
token for the registered HTTPS endpoint; custom Git returns no provider secrets.
The browser proxy must not expose this native credential endpoint.

Native CLI conflict comparison uses the exact current assignment/version, path, conflict
revision, and expected remote revision. The control plane grants only `config:compare`
through a signed `config_compare` credential, without creating a terminal. Assignment
and conflict changes invalidate access. Private comparison contents come from the host's
fenced conflict snapshots over the encrypted transport and never enter control-plane
storage.
