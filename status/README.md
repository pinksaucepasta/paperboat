# Status page

Public status for Paperboat, built to never show a wrong status.

| Part | What | Deploys |
|---|---|---|
| `worker.ts`, `lib/` | one Cloudflare Worker `paperboat-status`: static assets (the Astro build in `dist/`), the `/api/*` routes (probe ingest, state, incidents, RSS, subscribe), the D1 binding and the once-a-minute cron that decides every unit (`lib/decide.ts` is the state machine) | Workers Builds from git (root `status/`); `deploy.sh` for the first deploy, secrets and vars |
| `src/` | the page: Astro, static, light only, reusing the marketing site's styles, fonts and mark by import (`../marketing`) | with the Worker |
| `config/components.ts` | components, their units (checks or regions) and which vantage points report each | with the Worker |
| `../tools/probe/status_report.py`, `status_run.py` | the probe: user-path checks, own-internet check, signed report; `canary.py` calls it on oracle, `deploy/paperboat-status-probe.timer` runs it on anvit | on the probe hosts |
| `../admin/app/(panel)/status` | admin Status section, through `admin/lib/server/status-provider.ts` (the one seam to swap for a hosted provider); optional | with the admin panel |
| `drill.py`, `drill-results/` | stop/start drill against a staging stack and its results | manual |

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
`bun install && bun run test`; `bun run check` type-checks the Worker and the page. `wrangler dev --config dev/wrangler.staging.jsonc --local --test-scheduled` runs the staging catalog (trigger the cron with `GET /cdn-cgi/handler/scheduled`); `dev/seed.mjs` fills a demo database and `node dev/serve.mjs` serves `dist/` against it.
