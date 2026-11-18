#!/usr/bin/env bash
# Deploy the status page to Cloudflare with an account-scoped token (no zone permissions): the D1 database, the Worker (API + cron)
# on its workers.dev address, and the static Pages project, which gets STATUS_HOST as its custom domain.
# Secrets and the host come from files and the environment, never from this repo:
#   ~/.config/paperboat-status/cloudflare.env   CLOUDFLARE_API_TOKEN, CLOUDFLARE_ACCOUNT_ID
#   ~/.config/paperboat-status/deploy.env       STATUS_HOST (the page's hostname), NET_CHECK_URL, MAIL_ENDPOINT, MAIL_FROM
#   ~/.config/paperboat-status/probe-<vantage>.key  one signing key per probe host (oracle, anvit)
#   ~/.config/paperboat-status/admin.token      the admin panel's bearer token
#   ~/.config/paperboat-status/checks.json      the Worker's own checks: [{"unit","url","expect"?}]
#   ~/.config/paperboat-status/mail.key         the mail provider key (optional; without it subscribe and incident mail are off)
# Needs the account's workers.dev subdomain to exist (Workers & Pages > Overview > Subdomain, one click, once).
# usage: status/deploy.sh [--pages-only|--worker-only]
set -euo pipefail
cfg=${STATUS_CONFIG_DIR:-$HOME/.config/paperboat-status}
set -a; . "$cfg/cloudflare.env"; . "$cfg/deploy.env"; set +a
here=$(cd "$(dirname "$0")" && pwd); w=$here/worker
: "${CLOUDFLARE_API_TOKEN:?}" "${CLOUDFLARE_ACCOUNT_ID:?}" "${STATUS_HOST:?}" "${NET_CHECK_URL:?}"
wr() { (cd "$w" && npx --no-install wrangler "$@"); }
cf() { curl -fsS -H "authorization: Bearer $CLOUDFLARE_API_TOKEN" -H 'content-type: application/json' "$@"; }
acct=https://api.cloudflare.com/client/v4/accounts/$CLOUDFLARE_ACCOUNT_ID
sub=$(cf "$acct/workers/subdomain" | python3 -c 'import json,sys; print(json.load(sys.stdin)["result"].get("subdomain") or "")')
[ -n "$sub" ] || { echo "the account has no workers.dev subdomain yet: create it in the dashboard (Workers & Pages > Overview)" >&2; exit 1; }
api=https://paperboat-status.$sub.workers.dev
[ "${1:-}" = --pages-only ] || {
  wr d1 info paperboat-status >/dev/null 2>&1 || wr d1 create paperboat-status >/dev/null
  id=$(wr d1 info paperboat-status --json | python3 -c 'import json,sys; print(json.load(sys.stdin)["uuid"])')
  python3 - "$w" "$id" <<'PY'
import re, sys
w, i = sys.argv[1:]
s = open(f"{w}/wrangler.jsonc").read()
open(f"{w}/wrangler.deploy.jsonc", "w").write(s.replace('"database_id": "local"', f'"database_id": "{i}"'))
PY
  wr d1 execute paperboat-status --remote --file schema.sql >/dev/null
  keys=$(python3 - "$cfg" <<'PY'
import glob, json, os, sys
print(json.dumps({os.path.basename(p)[6:-4]: open(p).read().strip() for p in glob.glob(sys.argv[1] + "/probe-*.key")}))
PY
)
  put() { printf '%s' "$2" | wr secret put "$1" --config wrangler.deploy.jsonc >/dev/null; }
  put PROBE_KEYS "$keys"; put ADMIN_TOKEN "$(cat "$cfg/admin.token")"; put CHECKS "$(cat "$cfg/checks.json")"
  [ ! -f "$cfg/mail.key" ] || put MAIL_KEY "$(cat "$cfg/mail.key")"
  mail=(); [ ! -f "$cfg/mail.key" ] || mail=(--var "MAIL_ENDPOINT:$MAIL_ENDPOINT" --var "MAIL_FROM:$MAIL_FROM")
  wr deploy --config wrangler.deploy.jsonc --var "NET_CHECK_URL:$NET_CHECK_URL" \
    --var "PUBLIC_ORIGIN:$api" --var "PAGE_ORIGIN:https://$STATUS_HOST" "${mail[@]}"
  rm -f "$w/wrangler.deploy.jsonc"
}
[ "${1:-}" = --worker-only ] || {
  site=$(mktemp -d); trap 'rm -rf "$site"' EXIT; cp -r "$here/site/." "$site/"
  printf '// written by deploy.sh\nexport const API_BASE = "%s"\n' "$api" >"$site/config.js"
  sed -i "s#connect-src 'self'#connect-src 'self' $api#" "$site/_headers"
  wr pages project create paperboat-status --production-branch main >/dev/null 2>&1 || true
  wr pages deploy "$site" --project-name paperboat-status --branch main --commit-dirty=true
  cf -X POST "$acct/pages/projects/paperboat-status/domains" --data "{\"name\":\"$STATUS_HOST\"}" >/dev/null 2>&1 || true
  echo "If $STATUS_HOST is not active yet: dashboard > Workers & Pages > paperboat-status > Custom domains > Set up a custom domain > $STATUS_HOST > Activate (Cloudflare adds the DNS record itself)."
}
