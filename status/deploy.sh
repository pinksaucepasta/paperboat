#!/usr/bin/env bash
# Deploy the status Worker (API + cron + D1) to Cloudflare with an account-scoped token (no zone permissions; Workers Builds needs a
# permission this token lacks, so the Worker is deployed from here). The page is a git-connected Pages project (see below).
# Secrets and the host come from files and the environment, never from this repo:
#   ~/.config/paperboat-status/cloudflare.env   CLOUDFLARE_API_TOKEN, CLOUDFLARE_ACCOUNT_ID
#   ~/.config/paperboat-status/deploy.env       STATUS_HOST (the page's hostname), NET_CHECK_URL, MAIL_ENDPOINT, MAIL_FROM
#   ~/.config/paperboat-status/probe-<vantage>.key  one signing key per probe host (oracle, anvit)
#   ~/.config/paperboat-status/admin.token      the admin panel's bearer token
#   ~/.config/paperboat-status/checks.json      the Worker's own checks: [{"unit","url","expect"?}]
#   ~/.config/paperboat-status/mail.key         the mail provider key (optional; without it subscribe and incident mail are off)
# Needs the account's workers.dev subdomain to exist (Workers & Pages > Overview > Subdomain, one click, once).
# usage: status/deploy.sh
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
{
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
  mail=(); [ ! -f "$cfg/mail.key" ] || mail=(--var "MAIL_ENDPOINT:$MAIL_ENDPOINT" --var "MAIL_FROM:$MAIL_FROM")
  wr deploy --config wrangler.deploy.jsonc --var "NET_CHECK_URL:$NET_CHECK_URL" \
    --var "PUBLIC_ORIGIN:$api" --var "PAGE_ORIGIN:https://$STATUS_HOST" ${mail[@]+"${mail[@]}"}
  # All secrets in one bulk call from a private temp file; nothing is echoed.
  secrets=$(umask 077; mktemp); trap 'rm -f "$secrets" "$w/wrangler.deploy.jsonc"' EXIT
  python3 - "$cfg" "$keys" >"$secrets" <<'PY'
import json, os, sys
cfg, keys = sys.argv[1:]
read = lambda n: open(f"{cfg}/{n}").read().strip()
out = {"PROBE_KEYS": keys, "ADMIN_TOKEN": read("admin.token"), "CHECKS": read("checks.json")}
if os.path.exists(f"{cfg}/mail.key"):
    out["MAIL_KEY"] = read("mail.key")
print(json.dumps(out))
PY
  wr secret bulk "$secrets" --config wrangler.deploy.jsonc >/dev/null
  rm -f "$w/wrangler.deploy.jsonc"
}
# The page itself is not deployed from here: the Pages project paperboat-status is connected to the GitHub repo and builds status/ on every push
# that touches status/site or status/build.sh (build.sh, STATUS_API_BASE is a Pages environment variable). This only (re)attaches its domain.
cf -X POST "$acct/pages/projects/paperboat-status/domains" --data "{\"name\":\"$STATUS_HOST\"}" >/dev/null 2>&1 || true
