# Paperboat whiteboard

Remaining items are post-launch scope (user decision 2026-09-13).

## Generic UDP support — later

Recorded 2026-09-07 by Codex from the user's direction.

Add generic public and private UDP forwarding in a future scoped task. Preserve datagram
boundaries, bounded return-path mappings, expiry and cleanup. Public UDP needs assigned
endpoint ports, owner-authorized publication and abuse/traffic limits; private UDP needs
Paperboat-authorized encrypted native access. Application authentication remains protocol-specific.

UDP is deferred, not part of the current HTTP/HTTPS/WebSocket and raw TCP delivery tasks.
Internal QUIC support does not constitute generic UDP forwarding. Define actual client UX,
limits and acceptance before implementation; this note is not implementation/deployment evidence.


## CLI UX review and fixes — before customizable shortcuts

Recorded 2026-09-08 by Codex from the user's direction.

Review all CLI commands for clear, user-oriented names and consistent workflows before
adding customizable shortcuts. For example, consider `pb device manage` instead of
`pb device capabilities`: users should not need internal terminology to find device
controls. This example is a direction to evaluate, not a finalized command replacement.

Walk through the full command tree, help/discoverability, flags, defaults, output and error
messages. Simplify awkward or redundant commands around what users want to do. Update
affected CLI contracts, docs, completions, examples and agent interfaces together when
changes are selected. Settle the canonical UX first so customization builds on a coherent
interface rather than hiding confusing commands.

This is a future discussion/review item, not implementation authorization or a new launch gate.

## Paperboat SDK — discuss further

Recorded 2026-09-08 by Codex from the user's direction.

Explore a supported developer library with typed APIs, documentation, examples and stable
releases so developers can use Paperboat functionality inside their own products. Their
application provides the UI; users need not run `pb` commands or interact with Paperboat
directly. This means a proper SDK, not a wrapper that shells out to the CLI.

Candidate capabilities include device connectivity, terminal sessions and streaming I/O,
file transfers, previews/tunnels, and device/permission management. Discuss an embedded
connection runtime and management API client as a coherent developer interface, reusing
existing implementations. Decide initial scope, supported languages/platforms, enrollment,
application/user authorization, tenant isolation, key custody, lifecycle and compatibility
before committing to APIs. Invisible integration does not eliminate authorization needs.

This is an open discussion item, not a settled SDK design, implementation task or launch gate.

## Browser terminal over WebTransport — planned direction

Recorded 2026-09-26 at the user's direction. Use WebTransport for the browser terminal,
over HTTP/3 and QUIC. Prefer one WebTransport path over maintaining WebTransport and WSS
paths in parallel unless real browser/network testing shows a fallback is needed. WebTransport
does not have a TCP mode: if HTTP/3/QUIC is unavailable or blocked, the browser cannot use
this path. Any WSS fallback is a separate transport decision, not WebTransport over TCP.

This changes only the browser terminal carrier. Keep terminal rendering in the browser,
the existing one-use access ticket and View/Interact authorization, and end-to-end encryption
between browser and host through the edge. Do not route terminal content through the control
plane. Before selecting a fallback, test real browsers and restrictive networks, plus input,
output, resize, replay, background-tab recovery, reconnect, revocation and cleanup. This is
planning direction only, not implementation or deployment evidence.

## Remote desktop beta — deferred former Task 38

Moved from TRACKER.md on 2026-09-08 by Codex at user direction. Not scheduled,
not a launch requirement and not a Task 45 acceptance dependency.

When selected, integrate one existing third-party implementation and an explicitly
authorized native TCP/UDP adapter. Reuse Task 20/23 evidence and resolve current
prerequisites at activation. Define supported OS/provider/protocol versions, a named
owner and bounded effort before implementation. Do not build encoders, display/audio/input
stacks, drivers or a streaming engine, or expand core transport support implicitly.

Require actual pairing, per-resource control authorization, account/device isolation,
revocation, audit, cleanup, latency and degraded relay behavior on the declared matrix.
Userspace WireGuard alone does not prove compatibility. Preserve security and publish
honest support limitations; the beta must not block native SSH/editor workflows.

## PostHog product analytics — selected for future planning

Recorded 2026-09-13 by Codex at user direction. Add PostHog alongside Sentry: Sentry
explains failures; PostHog explains onboarding completion, feature adoption and where
users abandon workflows. This records the requested capability, not an SDK deployment
or a new launch gate. Schedule implementation explicitly when selecting this work.

Start with explicit dashboard events for onboarding steps, device enrollment completion,
tunnel/preview creation, team invitation acceptance and checkout completion. Track
success/failure outcomes and conversion funnels with small, documented event schemas.
Confirm successful actions from authoritative results rather than button clicks; payment
success must come from confirmed billing state. Avoid duplicate counting across browser
and server events. Collect only metadata needed for a named product question.

Use pseudonymous account/workspace identifiers where necessary to distinguish personal
and team usage; do not capture emails, device aliases/descriptions, command arguments,
terminal content, ENV values, credentials, file names/paths/payloads, HTTP request bodies,
inspector data or full URLs/query strings. Disable broad autocapture and session replay
by default; neither is part of this initial scope. Do not instrument user applications
served through PB tunnels. CLI/daemon analytics are not implicitly included in dashboard
instrumentation and require an explicit later decision.

Define reporting consent/settings, retention/deletion, access controls and event-volume
budgets before implementation. Honor applicable consent requirements; dashboard behavior
must remain functional when tracking is blocked, disabled or unavailable. Do not make
analytics an authorization, billing or availability dependency. Reuse existing user/team
identity boundaries without treating analytics identifiers as access credentials.

Verify event accuracy, deduplication, personal/team attribution, secret exclusion and
opt-out behavior against representative real dashboard workflows. Keep Sentry and PostHog
responsibilities distinct; do not build a duplicate analytics/reporting platform in PB.

## Official Paperboat agent skill — future implementation

Recorded 2026-09-13 by Codex at user direction. Provide an installable agent skill so
AI coding assistants can use Paperboat correctly through its supported CLI. This is
selected future planning, not implementation or a new launch gate; it does not require
the deferred embedded SDK or Codex/OpenCode remote-command integrations.

Teach device discovery and selection, capability/readiness checks, remote terminal/exec
and managed SSH, file transfers, previews/tunnels, ENV workflows and configuration sync.
Explain personal/team scope, exact resource selection, structured output, exit codes,
reconnect/retry behavior and actionable recovery. Follow the final CLI UX and published
help; avoid stale command examples or maintaining a second command/schema specification.
Keep initial instructions concise, with focused reference material for less common flows.

Agents use the user's existing authenticated PB identity and grants; the skill supplies
no extra authority. Require exact targets and respect user intent for remote execution,
public exposure, file replacement/deletion, sharing and team administration. Never expose
credentials, decrypted ENV values, terminal/file payloads or recovery material in logs or
reports. Handle unavailable devices and unsupported capabilities honestly; do not bypass
authentication or infer success from request submission alone.

Define distribution, installation, version compatibility and updates for supported agent
skill formats. Verify representative real workflows with an agent, including failures,
ambiguous device selection, permission denial and cleanup. Reuse the CLI implementation
and existing product evidence; no separate agent-only backend or bespoke protocol needed.

## Paperboat operator admin panel — backend implemented; frontend planned

Recorded 2026-09-13 by Codex at user direction. Add an operator-only admin area for
Paperboat staff, backed by capabilities in the existing `paperboat-server`; no separate
admin server. Team owner/admin/member controls remain in the existing team dashboard.
Backend implementation selected 2026-09-19 at user direction; frontend remains future work.
The owning evidence entry is “Operator admin backend” in `plans/TRACKER.md`.

Backend implementation extended 2026-09-19 to include the originally missed region/node,
backup and Sentry browsing scope. The authoritative API reference is
`server/docs/openapi.json`; operational semantics and activation steps are in
`server/docs/runbooks/operator-admin.md`. The frontend should consume these
APIs rather than adding authorization or state ownership in the browser:

- Full admin role; separate support, billing and operations staff capabilities.
  Session-bound WorkOS passkey reauthentication, fresh for every staff request.
- Paginated account/team/workspace/operation lists, exact email/account lookup,
  aggregate status and resource-specific audits; one PB reference for issue lookup.
- Account ban/unban with credential revocation; reversible team suspension with
  generation, paid-access, ENV and SMTP enforcement. Suspension preserves subscriptions;
  show unchanged billing status and the need for fresh authority after recovery.
- Relay drain/resume and tunnel disable/enable with explicit revision, target,
  evidence and retry key. Show accepted desired changes separately from runtime
  convergence, and show connector/edge/DNS/certificate observations per layer.
- Verified existing Stripe price registration, catalog activation/deactivation and
  immutable price/scope bindings; shared runtime bandwidth/device policy with revision
  conflicts. Stripe price creation, refunds and automatic plan migration are separate
  provider operations, not fabricated by these APIs. Deployment secrets remain outside
  editable runtime settings.
- Create/update/disable/enable hosted regions and enroll/update/drain/resume/retire
  nodes. Provision and rotate node-scoped runtime credentials; require explicit
  failure domains and revisions. Show runtime generation, last observation and registry
  expiry. Region disablement fences selection and runtime authority; enabling does not
  silently resume nodes. Enrollment manages existing machines, not cloud VM purchasing.
- Backup dashboard: newest run/failure, history, freshness, archive retention and exact
  backup-to-restore verification. Missing producer means not configured/unknown. Consume
  dedicated authenticated deployment evidence, never infer protection from database uptime.
  The Linux producer supports real dump/isolated restore, durable reporting retries,
  interruption cleanup and bounded retention; storage and scheduling are deployment-owned.
- Local reference feed and multi-project Sentry issue/detail/log/trace/metric views, with exact owner/reference
  resolution, project isolation, bounded safe fields and explicit provider errors. Keep
  ingestion enablement separate from read-only Sentry API access. Credentials remain in
  backend deployment secrets. These APIs browse evidence; issue triage remains in Sentry.
- Sentry cron monitors, per-environment check-ins and active incidents; metric detectors
  and their latest issues, plus accepted/filtered/rate-limited ingestion outcomes.
  Display disabled, missing, unsupported and stale evidence explicitly. Backend routes:
  `/v1/admin/sentry/monitors`, `/monitors/{slug}/checkins`, `/detectors` and `/outcomes`
  under the same Sentry prefix, with explicit allowlisted project selection.
- Hosted metric views distinguish a configured region/slot from population maxima;
  do not sum sampled gauges into fleet totals. Show observed bucket times, backup/restore
  state and age, real worker progress/stalls, SDK drop counters and signal enablement.
  Include immutable release/commit association and verified deployment status; publication
  alone is not proof of rollout. Native endpoints remain anonymous in fleet attribution.
- Billing/control recovery, signing/usage-key lifecycle and signed release authority
  retain the owning service's integrity and idempotency rules. Every operator action
  records intent/outcome and all mutation effects commit with their success audit.

Frontend work remains unimplemented: staff login/callback, permission-aware navigation,
search/status views, explicit impact/target confirmation, conflicts/retries, and the
Sentry/reference workflow. Live WorkOS passkeys, Stripe integration, backup storage/scheduling
and deployed runtime convergence still require deployment qualification. Real Sentry
SDK receipt and protected admin read-back were exercised with isolated PostgreSQL and
an identity-provider fixture; this does not qualify live WorkOS or the deployed admin UI.
An independent availability watchdog is deployed; in-process fleet rollout remains
separate. Exact evidence and remaining prerequisites are recorded in the tracker.


Cover account/team support, subscription and billing status/recovery, managed infrastructure
health, relay/tunnel status and authorized operational actions. Use the selected support API: `GET /v1/admin/support/{reference}?owner_id=...`
for audited, bounded failure timelines grouped by account and Sentry links, and
`GET /v1/support/{reference}` for account-owned safe status. The admin workflow accepts
one PB reference, resolves matching owners (with optional account filtering and a separate
unattributed group), and shows the initiating stage/source location, later failures, release, retention and
honest reporting status. Deep debugging stays in Sentry. Distinguish unconfirmed SDK
capture from confirmed delivery; expose unavailable/expired evidence without implying
success. Include reporting configuration/disablement, retention/deletion procedures,
dropped-report health and source-map/release readiness in admin operations scope.
Official native and hosted releases inject a default ingestion DSN and component
release; ordinary source builds have no Paperboat destination and require explicit
opt-in with their own Sentry configuration. Explicit disable remains available.
A public binary can be modified to copy its DSN, so this is a distribution default,
not proof of executable origin. Client-originated Sentry evidence is diagnostic input, never authorization evidence. Use the
implemented capability matrix and documented operational scope; ordinary team admins
do not become Paperboat operators. Self-hosted deployment-local operator authority
remains separate and is not granted by account pairing or a PB staff UI.

Enforce operator authorization on every backend request, strong authentication and audited
sensitive actions. Use least-privilege roles, explicit targets and clear impact/confirmation
for destructive actions. Record actor, action, target and outcome without secret content.
The admin URL or hidden navigation is not a security boundary. Operator status must not
automatically grant terminal/file access, user impersonation, decrypted ENV secrets,
private keys or recovery material. Avoid exposing credentials through configuration views.

Verify non-operator denial, role separation, cross-account support access boundaries,
audit completeness and safe retry/recovery for state-changing actions. Reuse authoritative
account/team, billing and infrastructure services; no alternate state store or bypass of
normal authorization/integrity rules. Scope any future emergency access explicitly.

## Open base server with private extensions — post-launch exploration

Recorded 2026-09-23 by Codex at the user's request. Planning only; the current server
and dashboards remain closed-source for launch.

Explore an independently usable open-source base server for personal self-hosting,
without teams, billing or enterprise features. Retain personal accounts, devices,
authentication, authorization and core Paperboat workflows; barebones must not mean
weakened security or dependence on the private server.

The closed-source server would import the base as packages and add teams and other
commercial/enterprise features in the same process. Share the core implementation;
avoid a separate service deployment or general plugin framework merely for this split.
The dashboard should discover server-provided capabilities and show only supported
features, while the server independently enforces all permissions and entitlements.

Before implementation, define the exact base feature set and license, inspect dependency
boundaries, and plan separation of ownership, authorization, database queries/migrations,
API composition and dashboard consumers. Verify both standalone base operation and the
complete private edition, including security and lifecycle behavior. This is substantial
architecture work requiring a scoped task, not a feature-flag-only packaging change.

## Tunnel-only mode without a Paperboat machine (and an official container image)

`pb tunnel run --token <token>` for a Docker container, Kubernetes pod or CI job, like
`cloudflared tunnel run --token`: nothing joins the account and nothing installs into
the system; it only forwards a tunnel to the app next to it. Needs a tunnel token with
rotation and one more admission path. Decision (2026-10-20): not for launch; until then
every tunnel runs on an enrolled Paperboat machine.

## Console talks to the local `pb` (browser ↔ local daemon bridge)

When the console runs on a computer that has `pb` signed in to the same account, the page
could use that local `pb` instead of our servers and edges. Candidate uses:
- terminals and exec straight over the peer-to-peer path (lower latency, no edge bandwidth);
- file transfers and Inbox downloads machine to machine, and drag-and-drop uploads from the page;
- private previews and private ports opened through the local `pb` (127.100.x.x) without
  browser sign-in at the edge;
- ENV vault unlock and config-sync conflict compare done by the local `pb` (keys never in the page);
- live machine status, diagnostics and `pb doctor` results shown from the local daemon;
- "Open in terminal / VS Code" buttons that hand off to the local `pb`.

Open questions before building: browser rules for a public page reaching a local port (Chrome
Private Network Access, Safari blocks), pairing so other websites can't use it, origin checks,
fallback to the normal path when no local `pb` is present, and not ending up with two code
paths per feature. Not for launch (user, 2026-10-21).

## Priority relays in more regions — after launch

Recorded 2026-10-23 at the user's direction (a Pro extra, not affordable yet). More managed relay
regions with priority capacity for paid plans, so relay-only paths stay fast far from today's edges.
Revisit once revenue covers the extra servers.

Container mode, recorded 2026-10-23 with the entry above: today `pb` runs in Docker only as a full
systemd container (`--privileged`, systemd as PID 1). The standard shape (Tailscale, cloudflared) is an
official image: `pb daemon` in the foreground as the main process, pairing from a one-time key in an
environment variable, state on a volume, no extra privileges. Covers Docker hosts, Kubernetes and CI.

## Fleets of temporary machines — right after launch (2026-11-10)

User decision: first work after launch; announced at launch. Many machines join with one
enrollment key and leave on their own when they disappear, then are used as a group (exec,
files, ENV, config). Details are kept outside this public file until it ships.

## Files in the team bucket — right after launch (user decision 2026-11-12)

Moved out of launch scope; the team bucket ships for session recordings only. Build plan: /home/ubuntu/pb-wt/spark/design-bucket-files.md.

- **Pending files for offline recipients** (was F25-08): Pending files (bucket use: files): a file sent to an offline recipient waits encrypted in the team bucket, client-side encrypted so neither Paperboat nor the bucket can read it; the recipient's Inbox shows it accept-first and it is delivered only after they accept; expired or declined files are deleted from the bucket.
- **Share a file → "Team storage"** (was F25-09): Share a file, destination "Team storage" (needs a team bucket): no machine has to be online; the browser encrypts the file, uploads it through a short-lived signed link, and the share link carries the decryption key after the # so neither Paperboat nor the bucket can read it; expiry and the Only-me rule apply; without a bucket the destination is not offered and links are served by the chosen machine.
- **Bucket files across OSes** (was X-32): Team bucket files across OSes (F25-06, F25-08, F25-09): with a team bucket for files, a file or folder sent from one OS to an offline recipient on another waits encrypted, is shown accept-first in the recipient's Inbox, and arrives byte-equal (sha256), with the same safe-name rules as X-14, after the recipient accepts; the client-side encryption written on one OS decrypts on every other (CLI on L, M, W and the browser); a Team storage share made in a browser on one OS downloads and decrypts in a browser on each other OS with the key only after the `#`, the Unicode or reserved file name is saved under a valid name on W, and expiry, the Only-me rule, decline and bucket cleanup behave the same; a bucket outage or wrong credential is named identically on every OS.

## Dual-stack edges (IPv6) — after launch (coordinator decision 2026-11-12)

Edges are IPv4-only at launch (no public IPv6, no AAAA for the preview/tunnel domain). IPv6-only visitors still
reach them through their carrier's NAT64/DNS64. Later: public IPv6 on every edge, wildcard AAAA records, owner IP
rules and `--host-header` checked with real IPv6 client addresses (the IPv6 part of cross-platform row X-20).

## Batched integration queue — after launch (user decision 2026-11-12)

Consider a bors-style queue that tests an actual combined batch once and bisects failed
batches, if native GitHub per-PR queue throughput proves insufficient. Before launch keep
GitHub's native queue with five concurrent speculative builds, rebase merges and the fast-PR /
heavy-merge-group split. Native merge limits do not amortize CI across PRs. Prior art:
[bors-ng batching](https://bors.tech/documentation/) and
[GitHub merge queue](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/configuring-pull-request-merges/managing-a-merge-queue).

## After launch: Go SDK and agent tools (removed from main 2026-11-12)

User decision 2026-11-12: the Go SDK (`sdk/go`) and `pb mcp` (with its device-smoke checks, CLI docs and marketing claims) were deleted from main until after launch. Restore from git history (the commit before this removal); the CHECKLIST section and command row below are kept verbatim, and the progress card ("SDK and agent tools", tools/progress.py) comes back with them.

## F24. Agent tools and Go SDK (PLAN 3b)

| ID | Scenario | L | M | W | Evidence |
|---|---|---|---|---|---|
| F24-00 | `pb mcp` serves MCP over stdio, bounds frames/calls, cancels active work on cancellation/disconnect and keeps stdout protocol-only. | [x] | [x] | [x] | M 2026-11-05 dev-f75226c10: mcp-smoke (MCP_SMOKE_EXEC, pb mcp on pbmac as source, target macl) all four rows PASS: stdio bounds/cancel/EOF, inventory 2/2, exec exact argv+stdin 65536 cap exit 7 deadline, send sha256 receipt + symlink refused SDK lane; mcp-stdio PASS: protocol-only stdout, one active tool, oversized frame closes session, cancellation and EOF stop the observed remote process. Nonce observer e83b672d respects worker PrivateTmp. Workspace: TestStdioCancellationAndDisconnectStopActiveTool, TestInvalidToolArgumentsNeverReachBackend, TestOversizedStdioFrameClosesSession. Full CLI vet/race CI 40327920695 PASS. Linux PASS 2026-10-26 on pbvm2, dev-c9ab1c14 (c9ab1c14b0853939a5cac0afb5383d16dd68fd5d), server 2026.10.23.102; fresh_pair_mcp.py with the official Go MCP client. Result: tools/device-smoke/results/f24-mcp-20261026-fixed.json. Both fresh machine IDs revoked exactly; containers removed. M/W not checked. W [x] 2026-11-10: fresh_pair_mcp.py --windows-target on hosted Windows Server runner winpool-12 (build dev-988ceece4, server 2026.11.10.30; source `pb mcp` on the Windows machine, target an hp container of the same account) RESULT PASS, run lanesdkba73286d24a94f8b, result spark/w12-mcp.json, report spark/w12-run.md: mcp-stdio (protocol-only stdout, frame bound, bounded concurrency, cancellation and EOF stop the remote process). |
| F24-01 | list_machines uses the selected CLI login/workspace, returns a bounded page and server total, without enrolling a new machine. | [x] | [x] | [x] | M 2026-11-05 dev-f75226c10: mcp-smoke (MCP_SMOKE_EXEC, pb mcp on pbmac as source, target macl) all four rows PASS: stdio bounds/cancel/EOF, inventory 2/2, exec exact argv+stdin 65536 cap exit 7 deadline, send sha256 receipt + symlink refused SDK lane; mcp-inventory PASS: authenticated Personal page returns 16/16 with the server total, bounded to 100, correct truncated flag and both fresh IDs. Workspace: TestMCPInventoryUsesBoundedAuthenticatedPage; full CLI vet/race CI 40327920695 PASS. Linux PASS 2026-10-26 on pbvm2, dev-c9ab1c14 (c9ab1c14b0853939a5cac0afb5383d16dd68fd5d), server 2026.10.23.102; fresh_pair_mcp.py with the official Go MCP client. Result: tools/device-smoke/results/f24-mcp-20261026-fixed.json. Both fresh machine IDs revoked exactly; containers removed. M/W not checked. W [x] 2026-11-10: fresh_pair_mcp.py --windows-target on hosted Windows Server runner winpool-12 (build dev-988ceece4, server 2026.11.10.30; source `pb mcp` on the Windows machine, target an hp container of the same account) RESULT PASS, run lanesdkba73286d24a94f8b, result spark/w12-mcp.json, report spark/w12-run.md: mcp-inventory 61/61 untruncated. |
| F24-02 | MCP exec uses exact argv and explicit stdin, bounds output without dropping the terminal outcome, preserves did-not-start/unknown and existing cancellation/reattach behavior. | [x] | [x] | [x] | M 2026-11-05 dev-f75226c10: mcp-smoke (MCP_SMOKE_EXEC, pb mcp on pbmac as source, target macl) all four rows PASS: stdio bounds/cancel/EOF, inventory 2/2, exec exact argv+stdin 65536 cap exit 7 deadline, send sha256 receipt + symlink refused SDK lane; mcp-exec PASS: empty/literal argv and NUL-containing stdin preserved, separate streams, combined output capped at 65,536 bytes with exit 7 retained, oversized stdin refused, deadline stops the remote process. Product fix 616d3fd1; TestExactArgvCWDEnvironmentAndSeparateStreams RED before/GREEN after; empty executable still refused. TestMCPExecPreservesNotStartedVersusUnknown and full CLI vet/race CI 40327920695 PASS. Linux PASS 2026-10-26 on pbvm2, dev-c9ab1c14 (c9ab1c14b0853939a5cac0afb5383d16dd68fd5d), server 2026.10.23.102; fresh_pair_mcp.py with the official Go MCP client. Result: tools/device-smoke/results/f24-mcp-20261026-fixed.json. Both fresh machine IDs revoked exactly; containers removed. M/W not checked. W [x] 2026-11-10: fresh_pair_mcp.py --windows-target on hosted Windows Server runner winpool-12 (build dev-988ceece4, server 2026.11.10.30; source `pb mcp` on the Windows machine, target an hp container of the same account) RESULT PASS, run lanesdkba73286d24a94f8b, result spark/w12-mcp.json, report spark/w12-run.md: mcp-exec (argv and stdin exact, 65536-byte output cap with exit 7, deadline stops the remote process, streams separate). |
| F24-03 | send_file accepts only an allowed local regular file, refuses symlink escape, snapshots before approval and calls the existing Automatic-mode send owner with receipts. | [x] | [x] | [x] | M 2026-11-05 dev-f75226c10: mcp-smoke (MCP_SMOKE_EXEC, pb mcp on pbmac as source, target macl) all four rows PASS: stdio bounds/cancel/EOF, inventory 2/2, exec exact argv+stdin 65536 cap exit 7 deadline, send sha256 receipt + symlink refused SDK lane; mcp-send PASS: receipt source/destination and SHA256 match the actual receiving Inbox file; symlink escape refused and no escaped artifact received. Workspace: TestMCPSendSnapshotsOnlyAllowedFiles, TestMCPSendDoesNotInheritTerminalDestination, TestMCPSendRefusesFIFOWithoutWaitingForWriter; full CLI vet/race CI 40327920695 PASS. Linux PASS 2026-10-26 on pbvm2, dev-c9ab1c14 (c9ab1c14b0853939a5cac0afb5383d16dd68fd5d), server 2026.10.23.102; fresh_pair_mcp.py with the official Go MCP client. Result: tools/device-smoke/results/f24-mcp-20261026-fixed.json. Both fresh machine IDs revoked exactly; containers removed. M/W not checked. W [x] 2026-11-10: fresh_pair_mcp.py --windows-target on hosted Windows Server runner winpool-12 (build dev-988ceece4, server 2026.11.10.30; source `pb mcp` on the Windows machine, target an hp container of the same account) RESULT PASS, run lanesdkba73286d24a94f8b, result spark/w12-mcp.json, report spark/w12-run.md: mcp-send (receipt sha256 verified, symlink escape refused). |

Go SDK rows below are planned acceptance checks; named checks to add are not passing evidence. Linux acceptance starts in L; platform columns remain unticked until their checks pass.

| F24-04 | Token-backed client sessions obtain exec descriptors without an enrolled source machine; exact target, workspace and peer claims are enforced, and exec-only authority stays exec-only. | [ ] | [ ] | [ ] | Server: TestSDKSessionExecSourceAuthorization + TestSignCredentialExecOperationBindsExactOperation (lane-b-test-pg); exact live token/session, target, workspace and exec scope, forged source/peer denied. waits for Lane R: destination admission and L/M SDK exec verification. |
| F24-05 | The same session-bound source obtains native private-access grants for an explicitly allowed TCP port, without borrowing exec or file scope. | [ ] | [ ] | [ ] | Server: TestSDKSessionPrivatePortAuthorization (lane-b-test-pg), TestScopedTokenRoute; exact private_access port scope, live session/token/workspace and creator authority; exec-only token denied. waits for Lane R: private TCP admission and L/M SDK verification. |
| F24-06 | The same session-bound source obtains file descriptors with explicit file scope, direction, target and transfer-mode claims; no fake source machine ID. | [ ] | [ ] | [ ] | Server: TestSDKSessionFileSourceAuthorization (lane-b-test-pg): files-only source, exact live client session/target/workspace, transfer_direction=send, operation_id and explicit mode/byte bound; forged source/peer, terminal substitution and exec-only authority denied. waits for Lane R: host admission of session source. |
| F24-07 | Token revocation reaches live session-bound exec, private TCP and file operations within the configured bound; disconnected revocation feeds fail closed at authorization lease expiry. | [ ] | [ ] | [ ] | Server: TestSDKLiveSessionRevocation PASS (lane-b-test-pg): all three access sessions revoked, peer_network/machine_access advance, private lease denied; signed host active_jtis preserves expired-admission revocations without retaining completed history; overflow fails closed. TestOfflineSessionRightsEqualTheDescriptorDecisionOnPostgres excludes token persistent rights. waits for Lane R: active_jtis request body, live teardown and feed-outage lease expiry. |
| F24-08 | SDK authorization and operation audit records identify creator, token, client session, workspace, target, operation and outcome, including denials/uncertainty, without command or file contents. | [ ] | [ ] | [ ] | Server: TestSDKSessionAuditAttribution PASS (lane-b-test-pg): creator/token/session/workspace/target/operation and authorized, denied, uncertain outcomes; exact signed destination outcome, idempotency and payload/credential exclusion. waits for Lane R: emit sdk_operation_ended via existing signed audit backlog for exec/private/file completion. |
| F24-09 | Session creation, descriptors and concurrent operations obey configured per-token/account limits; typed 429/Retry-After handling never starts an exec twice. | [ ] | [ ] | [ ] | Server: TestSDKSessionRateLimits PASS (lane-b-test-pg): token/account session and descriptor rates, concurrent exec/file/private reservations, admission-expiry retention, outcome release, ordinary-login and account isolation; TestValidationRejectsUnboundedSDKLimits PASS. Revocation feed capacity/history fixture PASS in 3.0s with all 10,001-row assertions retained. TestAPITokenBearerRequests checks typed 429 sdk_limit_reached + Retry-After. waits for Lane R: signed completion/revocation integration; SDK lane: TestSDKRetryAfterDoesNotRestartExec and L/M verification. |
| F24-10 | All eight shared leaves have one owner: diagnosticlog, wirejson, httptransport, serverclock, supportref, backoff, machinename and remotepath; CLI behavior remains unchanged. | [ ] | [ ] | [ ] | Owner: SDK lane, Lane A review. Check: byte-identical moves/import-only diffs, owning race tests, full shared/CLI CI, Darwin/Windows builds; decoder architecture guard remains in CLI contracttest. Workspace complete: all eight leaves pushed (last five e0b3c7a4, 5fa5ae2c, 08e7ab24, 7e818b90, 8bc7bce2); full shared/CLI CI 40280973537/40281027602/40281077634/40281114219/40281173829, owning race/vet and native/Darwin/Windows PASS; dependency closures exclude CLI/relay. L acceptance waits for the native CLI device batch. |
| F24-11 | R1/R2 peer leaves endpointidentity, fixedpacket, streamauth and peerquic live in shared with unchanged wire/authentication behavior and no CLI dependency. | [ ] | [ ] | [ ] | Owner: Lane R. Check: owning peer race/contract tests, full shared/CLI CI and Linux/macOS builds; import closure excludes cli/; peerquic follows diagnosticlog. |
| F24-12 | Relay grant/client contracts and nodetls move to shared before mesh; relay is a consumer, and shared never imports relay. | [ ] | [ ] | [ ] | Owner: Lane R (split/moves), Lane B (relay consumer). Check: module import graph has no shared/relay cycle; existing relay admission/carrier and mesh direct/fallback tests pass in shared/relay/CLI CI. |
| F24-13 | API, state, session and discovery owners are split in place then moved to shared, with injected credentials/clock/storage and the optional CredentialRejected hook retained. | [ ] | [ ] | [ ] | Owner: Lane A (source/freeze), SDK lane (agreed slices), Lane B (apiroutes prerequisite). Check: owning API/auth/state/discovery tests plus CLI login/refresh/revoke/pagination checks; shared import closure excludes CLI adapters. SDK slices moved: state 68e6a934, session 74c5c7ce, discovery 3f6e53cf, workspaces 9c608e0f; guard tools/check-shared-imports.sh; CI 41834751987 (shared, sdk/go, SDK macOS, cli PASS). Remaining: rest of api client and CLI login/refresh/revoke checks. |
| F24-14 | JWKS, identitybootstrap, trustedkeys, clientauthority, tailnet and native use the extracted shared API/state and relay contracts, preserving authenticated direct and relay connectivity. | [ ] | [ ] | [ ] | Owner: Lane A (JWKS boundary), Lane R (peer moves). Check: JWKS/identity/authority/native contract tests, direct/relay peer integration and shared/CLI CI; no shared imports of CLI or relay. |
| F24-15 | Runtime, exec framing and exec recovery have one reusable shared owner; the CLI consumes it and keeps operation-ID reattach, lost-start and cancellation behavior. | [ ] | [ ] | [ ] | Owner: Lane A and Lane R (boundary preparation), SDK lane (agreed moves). Check: existing exec lost-start/reattach/cancel/replay-gap tests against the shared owner and CLI; import graph excludes cmd/pb and local-daemon dependencies. |
| F24-16 | Native-private binding and TCP access adapters have one shared owner, without moving OS proxy/DNS/listener or host-announcement services into the SDK. | [ ] | [ ] | [ ] | Owner: Lane A and Lane R, SDK lane (agreed moves). Check: existing private-access binding/admission and port-denial tests against shared plus CLI private TCP checks; shared import closure excludes host services. |
| F24-17 | File protocol, transfer core and reusable send/receive orchestration move to shared with one recovery/receipt owner and unchanged Automatic/Unlimited path enforcement. | [ ] | [ ] | [ ] | Owner: Lane A (source/orchestration), Lane R (path enforcement), SDK lane (agreed moves). Check: existing manifest/offset-resume/receipt/size/path-switch tests against shared and CLI; no duplicate transfer constants or transport. |
| F24-18 | New/Close own isolated in-memory or explicit-directory state, never touch pb login or persist the API token; directory locks, atomic owner-only state, reopen/Logout and bounded idempotent shutdown work. | [x] | [x] | [x] | W [x] 2026-11-10: TestSDKClientStateIsolation, TestSDKStateDirectoryOwnership, TestSDKCloseCancelsAndJoins, TestSDKCloseBoundsStalledRevoke, TestSDKCloseReportsRevokeFailure (sdk/go) PASS on windows-latest (hosted runner, image windows-2025), Cross-platform run 42742255419 narrowed with -v at 5ca91a7d9 (main 7665c3114 + the -v switch): one --- PASS line per test, 0 SKIP, 0 FAIL. SDK lane; TestSDKClientStateIsolation, TestSDKStateDirectoryOwnership, TestSDKCloseCancelsAndJoins, TestSDKCloseBoundsStalledRevoke, TestSDKCloseReportsRevokeFailure in sdk/go (58080c2e); CI 41834751987 (shared, sdk/go, SDK macOS, cli PASS). |
| F24-19 | Machines pages authorized inventory and Machine resolves an ID or an exact unique name; ambiguous names return AmbiguousMachineError, and retries pin the resolved ID. | [x] | [x] | [x] | W [x] 2026-11-10: TestSDKMachineResolutionAndPagination (sdk/go) PASS on windows-latest (hosted runner, image windows-2025), Cross-platform run 42742255419 narrowed with -v at 5ca91a7d9 (main 7665c3114 + the -v switch): one --- PASS line per test, 0 SKIP, 0 FAIL. SDK lane; TestSDKMachineResolutionAndPagination in sdk/go (58080c2e) over shared/client/discovery (3f6e53cf); CI 41834751987 (shared, sdk/go, SDK macOS, cli PASS). |
| F24-20 | Exec preserves exact argv, explicit stdin, separate bounded output/backpressure, deadlines, typed errors and remote exit results; reconnect/lost start never reruns the command. | [ ] | [ ] | [ ] | Owner: SDK lane, Lane A/R integration. Check to add: TestSDKExecLifecycle on direct and relay paths, including lost start/cancel acknowledgement, writer failure, replay gaps and errors.Is/As. |
| F24-21 | Dial returns a deadline-capable private E2EE net.Conn for an authorized machine TCP port; HTTPClient pins that machine, verifies HTTPS and refuses redirects outside its authorized destination. | [ ] | [ ] | [ ] | Owner: SDK lane, Lane A/R integration. Check to add: TestSDKPrivateDialAndHTTP for direct/relay, port denial, deadline/cancel, redirect containment and Close; no arbitrary application-byte replay. |
| F24-22 | Send/Receive support explicit path or reader/writer forms for authorized offered artifacts, with bounded replay/spooling, stable transfer IDs, committed-offset resume and verified receipts. Automatic uses the server size limit; Unlimited never uses hosted relays. | [ ] | [ ] | [ ] | Owner: SDK lane, Lane A/R integration. Check to add: TestSDKArtifactTransferModes for size/batch boundaries, traversal/symlink/overwrite refusal, path-switch pause/resume, restart and byte/hash receipt; arbitrary remote reads wait for F24-25. |
| F24-23 | Expose returns an owned public URL for a local address or handler; Close removes the lease and serving workers, quotas apply, and edge-terminated TLS is documented distinctly from private E2EE. | [ ] | [ ] | [ ] | Owner: SDK lane (API/lifecycle), Lane B (server/edge), Lane A (device check). Check to add: TestSDKExposeLifecycle with a real public endpoint, failed/repeated Close, cancellation, lease removal and quota denial; phase after v1. |
| F24-24 | A runnable Go example uses a configured API token and a real Linux machine to resolve a name, exec, access a private TCP/HTTP service, transfer an offered artifact and Close, without enrolling the SDK process or running pb. | [ ] | [ ] | [ ] | Owner: SDK lane (example), Lane A (device run), Lane B/R (prerequisites). Check to add: sdk_example_linux scripted run recording build hash, direct/relay cases, output/receipt assertions and clean shutdown; no credentials in evidence. |
| F24-25 | Later remote filesystem reads/writes and Receive(arbitrary remote path) have their own explicit scope/protocol, remote-platform path/overwrite rules and bounded cancellation; they never fall back to exec. | [ ] | [ ] | [ ] | Owner: Lane A/B/R (contract and endpoints), SDK lane (consumer). Check to add: TestSDKRemoteFilesystemAuthorizationAndPaths, including file-only scope, traversal/symlink/overwrite denials and local/remote path separation; later phase. |
| F24-26 | Later Python/TypeScript bindings call the Go core through the chosen C ABI/native distribution, retaining the same transport, identity, typed outcomes and Close lifecycle. | [ ] | [ ] | [ ] | Owner: SDK lane. Check to add: sdk_bindings_conformance running the same machine/exec/private-access/artifact cases and cancellation/Close checks against Go and both bindings; no transport reimplementation. |

**Commands**

| ID | Command | Purpose | L | M | W | Evidence |
|---|---|---|---|---|---|---|
| C293 | `pb mcp` | Serve Paperboat tools over MCP stdio | [x] | [ ] | [x] | L [x] 2026-11-12: command_rows.py C293: --json refused before start, initialize + tools/list stdio handshake lists list_machines/exec/send_file, clean exit on EOF (Windows: PowerShell keeps stdin open, winhost MCP_HANDSHAKE). PASS on hp (pb dev-a427ba5f6r3). W [x] 2026-11-12: same row PASS on winpool-13 (Windows Server 2025 hosted runner, dev-36f2a7735). M waits for the Mac (one light run). Result tools/device-smoke/results/command-rows-c286-c293-20261112.txt. Flags: cli/docs/cli/pb_mcp.md. Behavior rows: F24-00 and following. |

## One exec costs about 12 round trips on a slow path — measured 2026-11-12 (rel lane)

Found by the macOS lane: `pb exec true` over a ~290 ms relayed path takes 3-4 s. Measured on two throwaway machines on one host
(warm daemon, direct path, `tc netem delay 290ms` on the client's egress so every dependent exchange costs 290 ms):

| | time |
|---|---|
| no delay | 1.4-1.9 s (about 1.7 s of local work: process start, daemon IPC, WireGuard and QUIC crypto) |
| all traffic +290 ms | 5.2 s, so about 12 dependent exchanges |
| only the peer path (UDP) delayed | 3.3 s, about 5-6 exchanges |
| only the control-plane path (TCP) delayed | 2.4 s, about 2-3 exchanges |

Timeline of one exec at +290 ms (packet capture, 4.65 s):
1. 0-0.4 s local: `pb exec` start, daemon IPC, warm machine lookup. 0 round trips.
2. 0.4-2.7 s control plane: descriptor request (1 RTT, creates the per-operation access session and its peer grant), then
   `network_refresh` (`acquireNativeRuntime` -> `RefreshForAdmission`): the client downloads the whole signed peer network document
   because the new grant's resource id is not in its cached copy (`hostRulesAdmission`, cli/internal/tunnel/peer_terminal.go:226).
   On the prod test account that document is about 265 KB, so TCP slow start alone adds about 4 RTT. A normal account with a few
   machines is a few KB: about 1 RTT.
3. 2.7-4.9 s peer: WireGuard handshake (1), stream open and admission (1), preface/token and welcome (1), application
   authenticate (1), exec start request and ack (1), started/exit events (1-2), close (1): 7-9 RTT.

Prior art: Tailscale SSH evaluates its policy locally from the cached netmap for every session (ssh/tailssh/tailssh.go
`evaluatePolicy`, no control-plane call unless the action is `check`); Teleport's node checks the user certificate it already holds
(lib/srv/authhandlers.go `PublicKeyCallback`), with the auth server only recording the session. Neither fetches a document or
creates a grant per command, and a multiplexed SSH exec adds 1-2 RTT.

What could be cut, largest first:
- The per-exec network refresh (2-5 RTT): return the signed scope of the new grant inside the descriptor response, or fetch the
  document conditionally (revision / ETag), or let two machines of one owner use a standing `exec` scope (the host already admits
  from its own `machine_access` scopes offline, F21-11) and keep the per-operation grant only for audit.
- The stream setup frames (3-4 RTT): send authenticate and the exec start request in the same flight as the preface (the server
  answers in order), instead of waiting for the welcome and the authenticate ack.
- The second WireGuard handshake seen in the capture (1.4 s and 2.8 s): a session closed after each exec makes the next exec
  handshake again; keeping the session for a few seconds would save 1 RTT on back-to-back commands.
Each is a protocol or authorization change on both ends: not done before the launch freeze (audit and revocation fences depend
on the per-operation grant). Re-measure with tools/device-smoke/perf_dataplane.py after any of them.

- Socket-based agent that Git-for-Windows ssh can reach (after launch): the managed agent is a Windows named pipe, which MSYS OpenSSH cannot use; until then Git runs through `git config --global core.sshCommand C:/Windows/System32/OpenSSH/ssh.exe` (pb ssh doctor prints it). A single ~/.ssh config cannot name both agent kinds without a `Match exec`.

## IDNA (non-ASCII) machine names — after launch (coordinator decision 2026-11-13)
Machine names are ASCII DNS labels: they appear in `<name>.<domain>`, `https://<port>.<name>.<domain>`, SSH host names and local
certificates, so `pb machine rename` refuses `zoë-build` with the name rule (shared/machinename). Allowing them needs IDNA
(punycode) in all of those paths and in the server's machinealias. Prior art: Tailscale MagicDNS also keeps ASCII host labels.
- Folder send (tree, empty dirs) — after launch. `pb send` takes regular files only today; a folder or symbolic link is refused with a clear message.
