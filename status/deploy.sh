#!/usr/bin/env bash
# Set up and update the status page's Cloudflare side with an account-scoped token (no zone permissions):
#   D1 database paperboat-status (schema), the cron Worker paperboat-status-cron (decides every minute), and the Pages project
#   paperboat-status (git-connected: every push that touches status/ builds the page and the Pages Functions API; this script only
#   sets the project's build settings, D1 binding, secrets and custom domain). Workers Builds needs a token permission we do not
#   have, so the cron Worker is deployed from here (it rarely changes).
# Inputs, all outside the repo, mode 0600, in ~/.config/paperboat-status (override with STATUS_CONFIG_DIR):
#   cloudflare.env  CLOUDFLARE_API_TOKEN, CLOUDFLARE_ACCOUNT_ID
#   deploy.env      STATUS_HOST (the page's hostname), NET_CHECK_URL (the cron vantage's own-internet URL), MAIL_ENDPOINT, MAIL_FROM
#   probe-<vantage>.key  one signing key per probe host      admin.token  the admin panel's bearer token
#   checks.json     the cron Worker's own checks [{unit,url,expect?,redirect?}]      mail.key  mail provider key (optional)
# usage: status/deploy.sh
set -euo pipefail
cfg=${STATUS_CONFIG_DIR:-$HOME/.config/paperboat-status}
set -a; . "$cfg/cloudflare.env"; . "$cfg/deploy.env"; set +a
here=$(cd "$(dirname "$0")" && pwd)
: "${CLOUDFLARE_API_TOKEN:?}" "${CLOUDFLARE_ACCOUNT_ID:?}" "${STATUS_HOST:?}" "${NET_CHECK_URL:?}"
wr() { (cd "$here" && npx --no-install wrangler "$@"); }
cf() { curl -fsS -H "authorization: Bearer $CLOUDFLARE_API_TOKEN" -H 'content-type: application/json' "$@"; }
acct=https://api.cloudflare.com/client/v4/accounts/$CLOUDFLARE_ACCOUNT_ID

wr d1 info paperboat-status >/dev/null 2>&1 || wr d1 create paperboat-status >/dev/null
id=$(wr d1 info paperboat-status --json | python3 -c 'import json,sys; print(json.load(sys.stdin)["uuid"])')
wr d1 execute paperboat-status --remote --file schema.sql >/dev/null

# The cron Worker: database id filled in a temporary config, its secret in one private bulk file, nothing echoed.
tmp=$(umask 077; mktemp -d); trap 'rm -rf "$tmp"' EXIT
sed "s/\"database_id\": \"local\"/\"database_id\": \"$id\"/" "$here/cron/wrangler.jsonc" >"$here/cron/wrangler.deploy.jsonc"
trap 'rm -rf "$tmp" "$here/cron/wrangler.deploy.jsonc"' EXIT
wr deploy --config cron/wrangler.deploy.jsonc --var "NET_CHECK_URL:$NET_CHECK_URL" >/dev/null
python3 -c 'import json,sys; print(json.dumps({"CHECKS": open(sys.argv[1]).read()}))' "$cfg/checks.json" >"$tmp/cron-secrets.json"
wr secret bulk "$tmp/cron-secrets.json" --config cron/wrangler.deploy.jsonc >/dev/null

# The Pages project: build settings, the D1 binding, secrets and vars of the Functions API.
python3 - "$cfg" "$id" "$STATUS_HOST" "${MAIL_ENDPOINT:-}" "${MAIL_FROM:-}" >"$tmp/pages.json" <<'PY'
import glob, json, os, sys
cfg, db, host, endpoint, sender = sys.argv[1:]
read = lambda n: open(f"{cfg}/{n}").read().strip()
secret = lambda v: {"type": "secret_text", "value": v}
plain = lambda v: {"type": "plain_text", "value": v}
keys = {os.path.basename(p)[6:-4]: open(p).read().strip() for p in glob.glob(f"{cfg}/probe-*.key")}
env = {"PROBE_KEYS": secret(json.dumps(keys)), "ADMIN_TOKEN": secret(read("admin.token")), "PUBLIC_ORIGIN": plain(f"https://{host}"), "STATUS_API_BASE": None,
       "PAPERBOAT_SITE_URL": plain("https://" + host.split(".", 1)[1])}  # the page links the Paperboat site (build-time)
if os.path.exists(f"{cfg}/mail.key") and endpoint and sender:
    env |= {"MAIL_KEY": secret(read("mail.key")), "MAIL_ENDPOINT": plain(endpoint), "MAIL_FROM": plain(sender)}
print(json.dumps({
    "build_config": {"build_command": "(cd ../marketing && bun install --frozen-lockfile) && bun run build", "destination_dir": "dist", "root_dir": "status"},
    # preview deployments (branches named in the project's source settings) get the same database and secrets: they exist to prove a build
    "deployment_configs": {k: {"d1_databases": {"DB": {"id": db}}, "compatibility_date": "2026-11-01", "env_vars": env} for k in ("production", "preview")},
}))
PY
cf -X PATCH "$acct/pages/projects/paperboat-status" --data @"$tmp/pages.json" >/dev/null
cf -X POST "$acct/pages/projects/paperboat-status/domains" --data "{\"name\":\"$STATUS_HOST\"}" >/dev/null 2>&1 || true
echo "Done. Push to main to build the page; check the domain: GET $acct/pages/projects/paperboat-status/domains"
