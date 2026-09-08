# CLI Security And Operations

The CLI sends `X-Paperboat-Client: paperboat` and
`X-Paperboat-Protocol: 1` on control-plane requests. Release builds also send a
versioned `User-Agent`. Unsupported protocols must be rejected with HTTP 426 or
`incompatible_client_version`; `pb` reports the upgrade message and does not
retry.

- Never put device, access, or refresh tokens in URLs or logs.
- A rejected access token requires `pb auth login`; there is no local-shell fallback.
- Upload failures are fail-open only for the affected paste. Image bytes and paths are never logged.
- For a stolen device, revoke its client session in the dashboard, then run `pb auth logout`.
- During outages, use `pb doctor`; never bypass the common Paperboat transport or expose a raw
  machine port. `pb ssh` is allowed only when its stream succeeds through the normal selected
  direct/relay/WSS carrier and terminates at the machine's system `sshd`.
- `pb codex` credentials, remote paths, command arguments, and Codex output are never
  written to logs. The remote environment owns its Codex login and configuration.

Production connection metrics are written as validated JSONL to
`observability.event_log_path`, or `telemetry.jsonl` beside the CLI config by
default. The file is restricted to mode `0600` and never contains network endpoints,
candidate addresses, credentials, private-content bytes, or local/machine paths. Opaque endpoint
IDs may appear only where the documented diagnostic contract requires them. Its configured size
limit bounds disk usage; the oldest accumulated events are truncated when the
next record would exceed that limit.

Terminal output compression is application framing shared by QUIC and WSS. Terminal
sequence, replay, queue, and PTY byte counters describe decoded user bytes; connector
and edge wire counters describe encoded bytes and therefore need not match. Repeated
terminal codec failures indicate an incompatible or corrupt peer: stop reconnecting,
retain only typed diagnostics and counters, and upgrade both runtime endpoints. Never
capture a terminal payload for diagnosis.

Each ended or replaced client transport connection records metadata-only
`terminal.compression` events for raw/Zstandard frame counts, decoded and encoded bytes,
decode nanoseconds, and typed failure count. These events intentionally omit project,
environment, session, request, and user identifiers and never include payload bytes.

The client output queue is bounded by 256 complete decoded events. Each event is bounded
by `MaxTerminalOutputBytes`, so its worst-case decoded admission is
`helperOutputQueueDecodedLimit` (about 64 MiB) regardless of encoded size. Output received
before attach completion is separately bounded to 64 events, 1 MiB encoded, and 1 MiB
decoded before any payload is queued.

Threat-model sign-off, cross-service metrics and audit events, artifact signing,
SBOM/provenance, and revocation propagation evidence remain release work in the
owning repositories.

Pushing a validated `YYYY.MM.DD.X` tag runs `.github/workflows/release.yml`. It builds the
five native assets, verifies their GitHub API-reported size and digest, signs the five TUF
asset targets, and atomically publishes `current.json`, installers, and TUF metadata. The
server publishes metadata only; installers and runtime updates download executable bytes from
the immutable GitHub release URLs recorded in the signed metadata.

General incident procedures are maintained in [runbooks.md](runbooks.md).
The complete v1 preview and tunnel workflow is in
[preview-tunnels.md](preview-tunnels.md), with focused recovery procedures in
[runbooks-preview-tunnels.md](runbooks-preview-tunnels.md).

## Machine runtime services

`make build` builds the single `bin/pb` executable. For a foreground development
run, use `bin/pb daemon --config /absolute/path/to/config.json`; another terminal
can use `bin/pb status --json`. SIGTERM drains local requests and closes the daemon's
transport leases before releasing its process lock. A crash leaves a stale socket;
the next daemon may remove it only after acquiring the exclusive owner lock.
Service-owned host, config and update roles run beneath `pb daemon`; `paperboatd`
is the logical service name. Platform package/layout qualification and packaged update
recovery remain the following migration gates.

`pb status` and `pb wait` use one per-user local daemon. When its owner-only Unix socket is
already healthy, commands only read the local API. If the socket is absent or refusing
connections, the CLI atomically installs and starts `paperboatd.service` under
the Linux user systemd manager or
`com.pinksaucepasta.paperboatd` under the macOS GUI launchd domain, then waits
up to five seconds for a validated snapshot. Permission, protocol-version, and invalid-state
failures never trigger service replacement. The service resolves the installed executable itself
and runs it with `daemon`, preserves an explicit config/server selection, uses the
canonical user state/runtime paths, and holds the process lock that authorizes stale-socket
cleanup. `pb uninstall` stops and unloads this service before removing its definition or
user state.

Active peer clients publish metadata-only `paperboat.transport-observation/v1` records to
the owner-only local API. Records contain a random process source ID, monotonic sequence,
machine ID, selected path category, the signed descriptor's relay region for relay-backed
paths, active lease count, and a maximum 30-second expiry; they never contain relay URLs,
endpoints, candidates, network fingerprints, credentials, or payloads.
The daemon rejects stale or replayed records, caps publishers and consumer counts,
aggregates concurrent processes, and expires crashed publishers. Closing a peer connection
publishes an immediate zero-consumer record. Control-plane inventory refreshes preserve
this local overlay, while the snapshot store alone assigns monotonic status generations.

For online current installations, the same bounded inventory refresh reads the
generation-fenced managed SSH target and active host-key set. A missing target reports SSH
as unavailable, a target without approved current-generation host keys reports degraded,
and only matching current generations plus healthy local managed-SSH integration report
ready. Authentication, authorization, and
malformed authority fail the refresh and preserve the last good snapshot rather than
guessing readiness.

Machine SSH names use the server-owned `alias` field, not display names. Aliases are
lowercase ASCII DNS labels and unique within an account while the machine is active.
Enrollment derives a stable label, avoids reserved Paperboat command and infrastructure
names, and allocates deterministic `-2`, `-3`, and later suffixes under account-scoped
transaction serialization. Local inventory rejects absent or malformed aliases.

Managed SSH uses the canonical OpenSSH host `<machine>.pprbt`; the former `.pprbt.dev`
form is not accepted. `pb ssh <machine>` delegates to the system OpenSSH client as
`<setup-user>@<machine>.pprbt`, where the setup user is the operating-system account that
ran `pb setup`. That registered user is an authorization boundary: an explicit username
is accepted only when it exactly matches the registered target, and any other username
fails before the SSH stream opens. The installed `Host *.pprbt` configuration uses the
same ProxyCommand, public selector for the credential-store-backed managed identity, and
strict host-key source for native `ssh`, `scp`, `sftp`, `rsync`, Git-over-SSH, and OpenSSH
forwarding. Native ecosystem commands should spell the registered user explicitly, for
example `scp file root@hn.pprbt:/tmp/file`.

### Native editor remoting

Native editors use the installed managed OpenSSH configuration. They must invoke the
system `ssh` client against the canonical host, not replace it with `pb ssh`: editor
backends need OpenSSH's normal command, dynamic-forwarding, file-operation, and reconnect
behavior. The daemon owns the `~/.ssh/paperboat_config` include, the per-machine
`<alias>.pprbt` target block, the Paperboat `ProxyCommand`, the managed identity agent,
and the generation-bound `KnownHostsCommand`.

The setup flow is:

1. Complete `pb setup`/`pb pair` and wait for the machine's runtime and SSH readiness.
   `pb machine list --json` exposes the server-owned alias; do not substitute the
   display name. `pb ssh doctor <machine>` checks OpenSSH parsing, the managed agent,
   current host-key authority, and the native SSH path.
2. In the editor's native SSH or remote-development connection flow, select
   `<registered-os-user>@<alias>.pprbt`. Keep the editor on its normal system OpenSSH
   executable so it reads the Paperboat include and preserves OpenSSH forwarding.
3. Allow the editor to start its remote backend in a task-owned workspace. Verify a
   file create/edit/save/reopen, one editor port-forward, and a reconnect after the
   Paperboat connection is interrupted. Independently verify the saved remote bytes.
4. If the editor reports a host-key change, authorization failure, or SSH readiness
   error, stop and run `pb ssh doctor`; never enable `StrictHostKeyChecking=no`, replace
   `KnownHostsCommand`, copy the managed private key, or expose the machine's SSH port.

`pb ssh doctor` checks prerequisites. Task 16's user-refined acceptance verifies the SSH
operations used by VS Code Remote-SSH directly through the native adapter. This establishes
the SSH compatibility boundary; it is not a claim that every editor lifecycle or deployed
account workflow has been qualified.

Remote-SSH 0.128.0's installed `package.json` and `out/extension.js` were inspected alongside
Microsoft's [Remote-SSH documentation](https://code.visualstudio.com/docs/remote/ssh) and
[troubleshooting guidance](https://code.visualstudio.com/docs/remote/troubleshooting).
Its default Linux path uses system SSH, disables PTY allocation (`-T`), feeds a shell
bootstrap through stdin and enables dynamic forwarding (`-D`). Local server downloads
can be copied with SCP. Optional socket mode uses `-L`; optional PTY allocation uses
ordinary SSH PTY support. `useExecServer` changes the remote backend/bootstrap, not the
underlying SSH operations. File editing runs in the remote backend over the forwarded
connection; it does not require an additional Paperboat file protocol.

| SSH requirement | Native-adapter evidence |
| --- | --- |
| Host selection, user/port, authentication and host-key enforcement | Managed config parse tests; real OpenSSH authentication, wrong-host-key rejection and revoked-access rejection. |
| Non-PTY bootstrap and command streams | Piped shell script, quoted path with spaces, detached backend process, binary stdin/stdout including EOF, separate stderr and exact remote exit status. |
| TCP forwarding | `-T -D … host sh` with a live bootstrap channel and four concurrent binary SOCKS channels; existing local TCP `-L` check. |
| File transfer | Existing real SCP upload and SFTP download with exact saved bytes. |
| Disconnect and fresh connection | Closing the native session terminates the SSH process; new SSH/SOCKS channels work, and the detached backend survives. |
| Optional Unix-socket forwarding and PTY | Real `-L` forwarding to a Unix socket with concurrent channels; explicit PTY allocation. |

Run the focused operation suite with `go test ./internal/peertransport/native -run
'^TestSystemOpenSSHOverNativeTailnet$' -count=1 -timeout=60s`; its `-race` run uses
`-timeout=90s`. The checks use real OpenSSH/sshd and authenticated native streams with
generated fixture identities. Production enrollment, managed-agent integration and
generation-bound `KnownHostsCommand` cutover remain separate Task 20 evidence.

| Editor/client | Intended first qualification | Status | Limitation |
| --- | --- | --- | --- |
| VS Code Desktop + Remote-SSH | Linux client/backend | SSH operations verified | VS Code 1.136.1 / Remote-SSH 0.128.0 also started a backend, saved/reopened a file and forwarded a port through the native fixture on Ubuntu24 amd64. The automation exited after forced disconnection; full editor reconnect is unverified. |
| JetBrains Gateway | Linux amd64 client to an approved Linux machine | Unqualified | Gateway's OpenSSH-config, host-key, backend, and reconnect behavior has not been measured. |
| Zed | Linux amd64 client to an approved Linux machine | Unqualified | No Zed client was available for the required end-to-end scenario. |
| Cursor | Linux amd64 client to an approved Linux machine | Unqualified | No Cursor client was available; VS Code compatibility cannot be inferred. |

The operation suite ran in the Linux arm64 workspace; the partial desktop scenario ran
with client and backend on Ubuntu24 amd64 under Xvfb. It does not establish a separate
HP-to-Ubuntu24 network scenario. Windows amd64 and macOS arm64 editor combinations remain
unqualified. Compatibility with the verified SSH operations is expected; universal
native-editor compatibility and full editor recovery are not claimed.

Machine runtime services may set `PAPERBOAT_HTTP_PROXY`, `PAPERBOAT_HTTPS_PROXY`, and
`PAPERBOAT_NO_PROXY` as administrator policy. These settings take precedence over the
standard process environment and native macOS proxy settings for all runtime-owned
control, WebSocket, update, artifact, and configuration traffic. Proxy URLs must use
`http` or `https`, contain a host, and contain no credentials, path, query, or fragment;
invalid policy prevents the runtime from starting. Paperboat never downloads or executes
network PAC/WPAD scripts and never accepts proxy credentials in configuration. For private
preview hostnames, hostd may install its own narrow local PAC rule that routes only those
names through the authenticated local Paperboat proxy.

`pb pair` installs the terminal host and its `runtime` connector. Configuration sync is a
separate per-user `__runtime-config` service and starts only while the local machine has an
assignment. Neither process initializes preview monitoring.

`pb preview <port|url|path>` and `pb tunnel --ephemeral <port|url|path>` use the same
handler and create one temporary server lease, then dispatch it to the
stable host runtime. The host runtime owns the machine-wide authenticated carrier, origin
probe, owner-session heartbeat, recovery, and cleanup. CLI processes observe the lease and
never create competing carriers or per-preview operating-system services.

`--background` transfers owner lifetime to the daemon only after readiness; its `--ttl`
defaults to 30 minutes, cannot exceed 24 hours, and never implies reboot restoration.
`pb preview list` and `status` read canonical server resources. `stop` and `delete` revoke the
lease and converges the route, carrier registration, and owner session. `--domain` attaches
a verified custom domain. `--private` uses the hostd-owned narrow local proxy/PAC route so
browser traffic carries no Paperboat credential or browser login state.

`pb tunnel doctor --bundle /absolute/output.zip` previews a bounded, redacted host-runtime
support bundle without returning source paths or secrets. Add `--write-bundle` to publish
the previewed file. Session-mode transitions, unpair, and uninstall revoke runtime authority
and retire affected preview routes.

Machine-control credentials renew in memory and are bound to the enrolled Ed25519 key and
installation generation. Reinstall, unpair, or machine revocation invalidates them. Never
copy a runtime state directory to another machine.

## Container hostd and updates

Hosted and self-hosted container deployments use the same split update boundary as native
hosts. `paperboat-hostd` stays alive and owns live workloads; the root-only
`paperboat-updated` process verifies TUF metadata, rotates root-owned artifacts in the
persistent release volume, and replaces only the runtime worker. Routine runtime and CLI
updates do not replace the container or close a hostd-owned stream.

Use the supplied [container deployment guide](../deploy/container/README.md) and Compose
definitions. Keep `/var/lib/paperboat` and `/workspace` persistent, distinct, and attached
only to the Paperboat container. The supplied definitions make the root filesystem read-only
and do not publish any host port. A pod eviction, image replacement, node failure, or an
administrator container restart is supervisor-class maintenance, not an invisible update.
