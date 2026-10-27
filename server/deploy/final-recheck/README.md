# Final re-check: wipe prod and rebuild the fixture

Run on the managed VPS from `/opt/paperboat/final-recheck/` (copy of this directory, mode 0700). Nothing here runs
by itself; the wipe needs the confirmation word.

| Script | Does |
|---|---|
| `verify-backup.sh [dump]` | Restores a dump into a scratch Postgres container (same image as prod, tmpfs) and compares row counts of the key tables with the live database. Exit 1 on any difference. |
| `export-config.sh <dump> <dir>` | From a scratch restore, writes the infrastructure rows a wipe keeps (see below) as COPY files. Prod is only read. |
| `import-config.sh <dir> [container] [db]` | Into a freshly migrated database: the config rows (triggers off), an orphan-row check over every foreign key, counts. |
| `rehearse.sh [dump]` | The whole rebuild in a scratch container against a restored copy of prod: migrate an EMPTY database with the live server image, import. Prod is only read. Run it before the wipe. |
| `wipe-and-rebuild.sh WIPE-PROD-DATABASE` | stop servers, fresh backup, verify it restores, export, drop and recreate `paperboat`, migrate with the live image, import, start the server, health checks. After the drop, any failure restores the backup. |

## What survives, what is wiped

Kept by restoring them from the verified backup, so the managed edges (hosts, containers, certificates on disk) are not
touched and keep their registration: `control_tunnel_nodes`, `selfhost_installations` (node credential hashes),
`control_usage_verification_keys`, `relay_subject_sets`, `edge_dns_provider_budgets`, `operator_accounts`,
`operator_bootstrap`, `billing_products`, `billing_operator_policy`, `client_update_policy`, `workos_event_cursor`, and the
two platform wildcard certificate records with their targets (re-issuing would spend Let's Encrypt rate limit). Secrets,
`.env`, `server.env`, WorkOS and Stripe settings and images are never touched.

Everything else is wiped: every machine, CLI session and token (each test machine must be uninstalled and re-paired),
ENV vaults, tunnels, previews, audit and usage history, every team and account. The final re-check uses only fresh
throwaway accounts; `tools/device-smoke/final_fixtures.py --email E [--member M ...]` calls the server's scoped
`/v1/test-fixtures` endpoint (bearer `PAPERBOAT_TEST_FIXTURES_TOKEN` from `server.env`, the one CI secret
`FINAL_FIXTURES_TOKEN`) and gives each its paid team after its first sign-in and prints `{"env": {"PAPERBOAT_SMOKE_TEAM": "<team>"}}`;
`--teardown` makes the other members inactive so the account can be deleted (the product deletes a team left with its
owner alone when the account is deleted). It is idempotent; the entitlement is granted as test data (team-pro, quantity
10, period now-1h .. now+90d), the same rows a completed team checkout leaves.

## Order

1. `./final-recheck/rehearse.sh` (must print `REHEARSAL-OK`).
2. Tell the test lanes to stop; wipe each device afterwards (the wipe makes every machine's server record disappear).
3. `./final-recheck/wipe-and-rebuild.sh WIPE-PROD-DATABASE`.
4. The runner signs up its throwaway accounts, calls `final_fixtures.py --email E` for each, and pairs the devices.
