# Sentry operational monitors

`tools/sentry-operations.py` manages only Paperboat-owned `pb-*` cron monitors
and metric issue detectors. It never creates email, chat, webhook, workflow, or
other external notification destinations. Cron alert targets, detector
actions, and detector workflow IDs are fixed to empty arrays; operators consume
the resulting incidents through Paperboat's authenticated admin APIs.

Copy `deploy/sentry-operations.example.json` outside the repository and set the
real Sentry organization and project slugs. The example uses Sentry's EU API
origin. The US origins `https://sentry.io/api/0` and
`https://us.sentry.io/api/0` are also accepted. The example cron monitors are
disabled because publication alone does not install a check-in producer. Change
both `producer_configured` to `true` and `status` to `active` only after the
named production job sends check-ins on that exact schedule. Detector thresholds
are initial production policy values and should be changed from measured traffic,
without changing their fixed query shapes.

`environment` is required and limited to `production`, `sentry-validation`, or
`development`; use the exact environment emitted by the deployment being
validated. Detector components are limited to the five Paperboat telemetry
components. Every detector has a critical `threshold` and a lower or equal
`recovery_threshold`; latency detectors must use a nonzero healthy latency so an
incident can recover while the service continues handling requests.

Create a token file readable only by its owner. The token needs read access for
planning and `alerts:write` (or the documented equivalent project/org write
scope) for apply. Do not put the token in the JSON file or an argument.

```sh
install -m 0600 /dev/null /secure/path/sentry-token
tools/sentry-operations.py plan --config /secure/path/sentry-operations.json --token-file /secure/path/sentry-token
tools/sentry-operations.py apply --config /secure/path/sentry-operations.json --token-file /secure/path/sentry-token
tools/sentry-operations.py verify --config /secure/path/sentry-operations.json --token-file /secure/path/sentry-token
```

`--token-stdin` is available for an orchestrator secret stream. Interactive use
disables terminal echo while reading. Requests ignore ambient HTTP proxies, do
not follow redirects, have a 10-second timeout by default, cap response bodies
at 1 MiB, and stop after ten pagination pages. Authentication and authorization
failures identify the missing access class without printing upstream response
bodies.

`plan` prints creates, updates, exact desired payloads, unchanged resources, and
owned `pb-*` resources omitted from the file. Omitted resources are reported but
never deleted. `apply` performs the plan and then reads everything back; it fails
if any desired field differs. `verify` is read-only and fails on drift. Review
the complete plan before apply, then retain the post-apply JSON as the change
record. These commands do not change Sentry subscription or paid-plan settings.

Sentry currently accepts `alert_rule.targets: []` when creating or updating a
cron monitor but omits targets from both list and detail readback. The tool marks
each existing cron as `reassert`, reports this limitation, and sends the empty
target list again on every apply. It verifies every other cron field from the
normalized readback. A successful apply therefore proves that Sentry accepted
the empty target request, while target readback remains unavailable; verify does
not mislabel that field as confirmed.

The request shapes follow Sentry's published API schema and pass local
mock-contract tests. The authorized validation apply created seven detectors and
the availability cron; detector detail readback matched the desired fixed
configuration. Cron creation and reassertion accepted empty targets, subject to
the target-readback limitation above.

## Independent availability watchdog

Build the watchdog for its independent Linux amd64 host and install only the
binary, public target configuration, protected ingestion environment, and
systemd units. The watchdog does not use a Sentry API token.

```sh
mkdir -p .build/diagnostics
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -o .build/diagnostics/paperboat-watchdog ./cmd/paperboat-watchdog
artifact_sha=$(sha256sum .build/diagnostics/paperboat-watchdog | awk '{print $1}')
artifact_sha12=$(printf '%s' "$artifact_sha" | cut -c1-12)
release="paperboat-watchdog:20260919.$artifact_sha12"
sudo install -d -m 0755 /opt/paperboat/diagnostics /etc/paperboat-diagnostics
sudo install -m 0755 .build/diagnostics/paperboat-watchdog /opt/paperboat/diagnostics/paperboat-watchdog
sudo install -m 0644 deploy/paperboat-watchdog.service /etc/systemd/system/paperboat-watchdog.service
sudo install -m 0644 deploy/paperboat-watchdog.timer /etc/systemd/system/paperboat-watchdog.timer
sudo install -m 0644 /dev/stdin /etc/paperboat-diagnostics/watchdog.json <<'JSON'
{"targets":[{"monitor":"pb-control-availability","url":"https://api.pprbt.dev/readyz"}]}
JSON
sudo install -m 0600 /dev/null /etc/paperboat-diagnostics/watchdog.env
sudoedit /etc/paperboat-diagnostics/watchdog.env
```

Put exactly these values in `watchdog.env`, substituting the protected ingestion
DSN and the computed release printed by `printf '%s\n' "$release"`. Do not put a
Sentry API token in this file.

```text
PAPERBOAT_SENTRY_ENABLED=true
PAPERBOAT_SENTRY_DSN=<watchdog project ingestion DSN>
PAPERBOAT_SENTRY_RELEASE=paperboat-watchdog:20260919.<first-12-artifact-sha256>
PAPERBOAT_SENTRY_ENVIRONMENT=development
```

Check the installed paths and then enable the timer:

```sh
sudo systemd-analyze verify /etc/systemd/system/paperboat-watchdog.service /etc/systemd/system/paperboat-watchdog.timer
sudo systemctl daemon-reload
sudo systemctl start paperboat-watchdog.service
sudo systemctl enable --now paperboat-watchdog.timer
systemctl list-timers paperboat-watchdog.timer
sudo journalctl -u paperboat-watchdog.service --since '15 minutes ago'
```

The service uses a transient unprivileged identity and needs outbound DNS and
HTTPS only. A successful run prints one bounded JSON count. A probe failure exits
nonzero after completing the Sentry check-in, while absence of the timer itself
becomes a missed one-minute cron check-in. Roll out the production target and
environment only after its corresponding Sentry monitor exists and the
independent host can reach the production readiness URL.

The server's optional process-owned health monitor is separate. Configure
`PAPERBOAT_SENTRY_HEALTH_MONITOR` only for its provisioned one-minute monitor.
Set `PAPERBOAT_WORKER_STALL_TIMEOUT` to at least one minute and ensure the cron
monitor's check-in margin is at least that stall timeout, so normal startup and
worker diagnosis do not create a premature missed incident.

## Backup and restore schedules

Use the existing `paperboat-backup` commands and protected PostgreSQL/reporting
files described in `docs/runbooks/operator-admin.md`. A deployment scheduler may
run `backup` daily, `verify` weekly against the exact successful backup run,
`report-retry` before both, and `cleanup` after successful reporting. Enable
`pb-backup-daily` and `pb-restore-weekly` only after those jobs are installed and
their dedicated report token is accepted by
`/v1/operations/backups/reports`. Do not enable the Sentry schedules merely
because the monitor definitions exist; the API and this repository do not
install backup storage, PostgreSQL authority, or a scheduler.

The scheduler should invoke the binary directly with protected environment files;
these are the concrete command shapes:

```sh
mkdir -p .build/diagnostics
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -o .build/diagnostics/paperboat-backup ./cmd/paperboat-backup
sudo install -m 0755 .build/diagnostics/paperboat-backup /opt/paperboat/diagnostics/paperboat-backup

PAPERBOAT_BACKUP_REPORT_TOKEN_FILE=/etc/paperboat-diagnostics/backup-report.token \
PGSERVICEFILE=/etc/paperboat-diagnostics/pg_service.conf \
PGPASSFILE=/etc/paperboat-diagnostics/pgpass \
/opt/paperboat/diagnostics/paperboat-backup report-retry --database primary --maintenance-service paperboat-verify --storage /var/lib/paperboat-backup --report-url https://api.pprbt.dev/v1/operations/backups/reports

/opt/paperboat/diagnostics/paperboat-backup backup --database primary --source-service paperboat-backup --maintenance-service paperboat-verify --storage /var/lib/paperboat-backup --report-url https://api.pprbt.dev/v1/operations/backups/reports
/opt/paperboat/diagnostics/paperboat-backup verify --database primary --maintenance-service paperboat-verify --backup-run EXACT_SUCCESSFUL_BACKUP_RUN_ID --storage /var/lib/paperboat-backup --report-url https://api.pprbt.dev/v1/operations/backups/reports
/opt/paperboat/diagnostics/paperboat-backup cleanup --database primary --maintenance-service paperboat-verify --storage /var/lib/paperboat-backup --report-url https://api.pprbt.dev/v1/operations/backups/reports
```

The scheduled service must supply the three protected environment variables to
every invocation; they are shown once above only to keep the examples readable.
Keep the report token, libpq service file, and password file mode 0600. Capture
the successful backup command's JSON `run_id` for the weekly verify command.

## Applied deployment and acceptance (2026-09-19)

`deploy/sentry-operations.pprbt.json` is the applied configuration for organization
`pprbt`, project `go` on the EU API. Seven production error/latency detectors are
configured. `pb-control-availability` receives one-minute check-ins from the
Hetzner systemd watchdog. The currently deployed API identifies as development,
so its check-ins use that environment; this is not a production product rollout.
The watchdog is independent of the application process but shares its host.
Sentry detects missing check-ins if that host stops reporting.

Installed paths are `/opt/paperboat/diagnostics/paperboat-watchdog`,
`/etc/paperboat-diagnostics/watchdog.json`, the mode-0600 ingestion environment
`/etc/paperboat-diagnostics/watchdog.env`, and the two systemd units above.
Artifact SHA-256:
`0c1f3fd8a0a0558762bd1a8380a514bec0347c5244f59e2e21174a23c5188768`;
release `paperboat-watchdog:20260919.0c1f3fd8a0a0`. No read/write Sentry API token
is installed on the watchdog host. Keep the timer enabled during token rotation;
the API token and ingestion DSN have separate purposes.

Connected acceptance observed failed, missed and healthy cron check-ins, recovery
of the monitor incident, and a latency issue opening then resolving. Production
backup, restore and in-process health monitors remain disabled because their
scheduled/deployed producers are not yet installed. Do not interpret these
configured definitions as successful backups or healthy workers.

The operator API exposes monitors and check-ins, metric detectors, and ingestion
outcomes under `/v1/admin/sentry`. Ingestion outcomes distinguish accepted,
filtered, invalid and rate-limited categories; they are not billing usage.
`region` plus `instance` selects a bounded hosted slot for metric queries.
Population maxima, interval maxima and observed timestamps are explicit; absent
samples are unknown, never manufactured zeroes or current health.

Product deployment still requires a reviewed immutable release, protected per-service
reporting configuration, operator read credentials and production identity setup.
Backup protection additionally requires provisioned archive storage, PostgreSQL
backup/restore authority, scheduler and report credentials. Install those producers
and prove their reports/check-ins before enabling the corresponding cron monitors.
