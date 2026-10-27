# Signed release deployment

The native release build requires the `SENTRY_DSN` repository variable: `tools/build-release-asset.sh`
validates it and embeds it together with the exact release identity (`paperboat:<version>`).
Missing or malformed configuration fails the build; the API read/write token is never
embedded. The service container images take per-service variables (`SENTRY_DSN_SERVER`,
`SENTRY_DSN_RELAY`, `SENTRY_DSN_TUNNEL`); a malformed value fails the image build and an empty one
leaves reporting disabled. Source code contains no default ingestion DSN,
and ordinary builds remain disabled until explicitly configured for their own Sentry.
The release DSN is public in shipped binaries, so these are distribution defaults,
not authentication of the executable sending an event. Explicit runtime disable and
custom destination overrides remain supported. Previously published artifacts do not
change until a new release is built and installed.

The release workflow does not talk to the Sentry API. After the release is published, an
operator may run `tools/sentry-release.py release` (with `SENTRY_ORG`, `SENTRY_PROJECT` and
`SENTRY_AUTH_TOKEN` or `SENTRY_AUTH_TOKEN_FILE`, plus `--release`, `--repository`, `--commit` and
`--source-url`) to create and finalize the release in Sentry and associate the repository and exact
source commit. Use the identifier the binaries embed (`paperboat:<version>` for `pb`).

Artifact publication does not create a Sentry deploy. A rollout owner may run
`tools/sentry-release.py deploy` only after the rollout's own checks succeed. The
tool also requires the deployed service's HTTPS `--readiness-url` to return 200
and JSON whose `release` field exactly matches the release being recorded.

Paperboat updates are signed publication transactions. The host updater accepts
only metadata obtained through the embedded TUF root and the threshold-signed
TUF roles. `tools/release-plan` creates deterministic inputs for the signer and
the release journal; it is not a second runtime update source. After
publication, the copy in signed TUF target metadata is authoritative.

## Publication inputs

Build the selected canonical native assets (`pb-linux-amd64`, `pb-linux-arm64`, `pb-darwin-arm64`) in a clean absolute directory. Omitted platform targets retain their existing signed versions and policies:

```sh
go run ./tools/release-plan manifest \
  -version 2026.08.31.1 \
  -source-commit 0123456789abcdef0123456789abcdef01234567 \
  -toolchain go1.27.3 \
  -artifacts /absolute/path/to/qualified-assets \
  -output /absolute/path/to/manifest.json

go run ./tools/release-plan plan \
  -manifest /absolute/path/to/manifest.json \
  -policy-revision 7 \
  -severity routine \
  -cohort-seed release-2026.08.31.1 \
  -output /absolute/path/to/deployment-plan.json

go run ./tools/release-plan validate \
  -manifest /absolute/path/to/manifest.json \
  -plan /absolute/path/to/deployment-plan.json \
  -artifacts /absolute/path/to/qualified-assets
```

The signer consumes and validates those files before writing targets metadata:

```sh
go run ./tools/tuf-repository publish \
  -repository /absolute/path/to/tuf-production \
  -version 2026.08.31.1 \
  -artifacts /absolute/path/to/qualified-assets \
  -manifest /absolute/path/to/manifest.json \
  -deployment-plan /absolute/path/to/deployment-plan.json \
  -rollout-revision 7 \
  -severity routine

go run ./tools/tuf-repository verify-published \
  -repository /absolute/path/to/tuf-production
```

Publication embeds `manifest_sha256`, `deployment_plan_sha256`, and the static
deployment policy in every signed release-index target. The publisher checks
that targets from the same release carry consistent policy bytes and that the
selected artifact manifest matches each TUF length and SHA-256. Changing a policy or artifact
after signing invalidates the TUF role signatures and is rejected by
`verify-published`.

The signed policy contains rollout cohorts, canary requirements, bounded drain
and stability windows, rollback triggers, quarantine duration, and the maximum
deferral for each severity. Its `rollout_state` is one of `active`, `paused`,
or `quarantined`; only active policies are eligible for automatic rollout. A
`/healthz` path is only the probe path. The policy requires edge, connector,
route, and origin readiness, so process existence or a local endpoint alone
cannot pass the gate. `promote`, `pause`, and `quarantine` re-sign this same
policy for every target with a higher policy revision. There is no separate
unsigned rollout file.

## Runtime gate binding

The signed policy does not contain a machine's current process, session,
configuration, or route generations. The stable host daemon resolves those
values immediately before each gate call. A provider input can be generated for
that exact target:

```sh
go run ./tools/release-plan provider \
  -plan /absolute/path/to/deployment-plan.json \
  -target /absolute/path/to/current-target.json \
  -transaction-id update_20260831_01 \
  -previous-version 2026.08.30.1 \
  -previous-manifest-sha256 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef \
  -rollback-trigger edge_canary \
  -output /absolute/path/to/provider-input.json
```

`current-target.json` contains only public identity and monotonic fences:
machine, account, host, tunnel, connector, edge node, failure domain, process
epoch, session generation, configuration generation, and route generation. A
provider input repeats this complete binding in its canary, drain, stability,
and rollback requests. Credentials, authorization headers, local paths, and
URLs have no representation in the type.

The updater must reject an input whose target tuple changes between phases. A
reconnect, route replacement, or configuration replacement therefore obtains a
fresh provider input rather than reusing a stale one.

Manual updates require approval of the exact verified download.
`pb update check` reads signed metadata. `pb update download` stages the candidate
without executing it; `pb update --approve <candidate-id>` starts installation after
review. Official installations also have scheduled automatic updates on by default
(`pb update settings`): the verified candidate is downloaded ahead of the daily
maintenance time (04:00 machine-local) and installed then without a prompt. Custom
builds can neither check nor install. Interactive `pb update` shows the downloaded version, platform, size and digest
before its default-no confirmation. Changed signed metadata or staged bytes invalidate
approval. Ordinary updater restart preserves the download without activating it.

Approved installation restarts services and interrupts active connections. Terminals
and transfers use their existing reconnect/recovery behavior afterward; seamless work
preservation is not promised. The native transaction retains authenticated process
readiness, bounded monitoring and policy-authorized rollback, without requiring a
parallel worker, terminal-drain grant or a separate maintenance approval owner.

On Linux and macOS, the existing update transaction replaces the verified executable,
restarts the fixed native host service, and adopts its authenticated worker identity and
persistent epoch before checking stability. Restart and recovery have signed timeout
bounds. A failed cutover restores an artifact still permitted by the update trust policy
and restarts that previous host runtime before completing recovery.

## Journal and operator actions

Initialize a crash-safe deployment state before starting work:

```sh
go run ./tools/release-plan state-init \
  -plan /absolute/path/to/deployment-plan.json \
  -transaction-id update_20260831_01 \
  -previous-version 2026.08.30.1 \
  -now 2026-08-31T09:00:00Z \
  -output /absolute/path/to/update-state.json
```

Advance only through the ordered phases. Failed canaries, drains, activation,
or stability checks enter quarantine or rollback; no command silently skips a
phase:

```sh
go run ./tools/release-plan advance -state /absolute/path/to/update-state.json \
  -event download_started -now 2026-08-31T09:00:01Z
go run ./tools/release-plan advance -state /absolute/path/to/update-state.json \
  -event candidate_validating -now 2026-08-31T09:00:02Z
go run ./tools/release-plan advance -state /absolute/path/to/update-state.json \
  -event candidate_ready -now 2026-08-31T09:00:03Z
```

Quarantine and revocation outputs are bounded, typed, and free of binary
paths, credentials, and URLs:

```sh
go run ./tools/release-plan quarantine \
  -state /absolute/path/to/update-state.json \
  -now 2026-08-31T09:00:04Z \
  -output /absolute/path/to/quarantine.json

go run ./tools/release-plan revoke \
  -state /absolute/path/to/update-state.json \
  -reason "operator revoked" \
  -now 2026-08-31T09:00:05Z \
  -output /absolute/path/to/revocation.json
```

Routine deferrals are bounded to seven days, security deferrals to 24 hours,
and critical deferrals to one hour. Security and critical deferrals require an
explicit approval identifier. The signed plan and state journal remain the
source of the effective policy; command output is only a durable projection or
operator evidence.
