# Status page

Public status for Paperboat, built to never show a wrong status.

| Part | What | Deploys |
|---|---|---|
| `worker/` | Cloudflare Worker + D1: signed probe ingest, the state machine (`src/decide.ts`), incidents, RSS, email subscribe, a once-a-minute cron that also runs the Worker's own checks | `deploy.sh` |
| `site/` | static page (Cloudflare Pages, git-connected, `build.sh`) | every push touching `status/site` |
| `config/components.ts` | components, their units (checks or regions) and which vantage points report each | with the Worker |
| `../tools/probe/status_report.py`, `status_run.py` | the probe: user-path checks, own-internet check, signed report; `canary.py` calls it on oracle, `deploy/paperboat-status-probe.timer` runs it on anvit | on the probe hosts |
| `../admin/app/(panel)/status` | admin Status section (optional: says so without STATUS_API_URL and STATUS_ADMIN_TOKEN) | with the admin panel |
| `drill.py` | stop/start drill against a staging stack | manual |

## Rules (user rules; `worker/src/decide.ts` and its tests are the one implementation)
1. A unit is red (major) only when two independent vantage points each fail 3 checks in a row.
2. A probe host checks its own internet first (and again after any failure); if that fails it sends a heartbeat and no results.
3. A bad unit is green again only after 5 passes in a row on two vantage points (one if only one reports).
4. Degraded: two vantage points slow (over the unit's threshold) or failing less than red; a component with some but not all units red.
5. No result from any vantage point for 3 minutes: "Status data delayed", never green. The page shows "Unknown" when its fetch fails.
6. Incidents and maintenance are written by an operator in the admin panel; subscribers get email through the Resend-style mail API; `/api/feed.xml` is RSS.
7. Probe writes are HMAC-signed per vantage point (`x-status-signature` over `vantage\nts\nbody`, 2 minute skew); admin calls need the bearer token.
8. 90-day bars come from per-minute counts of component states (`uptime_day`), maintenance minutes excluded.

## Local run
`cd worker && bun install && bun run test`; `wrangler dev --config wrangler.staging.jsonc --local --test-scheduled` runs the staging catalog; trigger the cron with `GET /cdn-cgi/handler/scheduled`. `node dev-server.mjs 8788 http://127.0.0.1:8787` serves the page against it.
