#!/usr/bin/env bash
# Set up and update the status Worker on Cloudflare with an account-scoped token (no zone permissions): the D1 schema, the page build, the
# Worker (static assets + /api + cron), its vars and secrets, and its custom domain. Day to day the Worker is deployed by Workers Builds from
# git (see the private runbook); this script is for the first deploy, for secrets/vars changes, and as the fallback when that is not connected.
# Inputs, all outside the repo, mode 0600, in ~/.config/paperboat-status (override with STATUS_CONFIG_DIR):
#   cloudflare.env  CLOUDFLARE_API_TOKEN, CLOUDFLARE_ACCOUNT_ID
#   deploy.env      STATUS_HOST (the page's hostname), NET_CHECK_URL (the cron vantage's own-internet URL), MAIL_ENDPOINT, MAIL_FROM
#   probe-<vantage>.key  one signing key per probe host      admin.token  the admin panel's bearer token
#   checks.json     the cron's own checks [{unit,url,expect?,redirect?}]      mail.key  mail provider key (optional)
# usage: status/deploy.sh
set -euo pipefail
cfg=${STATUS_CONFIG_DIR:-$HOME/.config/paperboat-status}
set -a; . "$cfg/cloudflare.env"; . "$cfg/deploy.env"; set +a
here=$(cd "$(dirname "$0")" && pwd)
: "${CLOUDFLARE_API_TOKEN:?}" "${CLOUDFLARE_ACCOUNT_ID:?}" "${STATUS_HOST:?}" "${NET_CHECK_URL:?}"
wr() { (cd "$here" && npx --no-install wrangler "$@"); }
cf() { curl -fsS -H "authorization: Bearer $CLOUDFLARE_API_TOKEN" -H 'content-type: application/json' "$@"; }
acct=https://api.cloudflare.com/client/v4/accounts/$CLOUDFLARE_ACCOUNT_ID

wr d1 info paperboat-status >/dev/null 2>&1 || { wr d1 create paperboat-status >/dev/null; echo "created the D1 database: put its id in wrangler.jsonc and rerun" >&2; exit 1; }
wr d1 execute paperboat-status --remote --file schema.sql >/dev/null
(cd "$here/.." && bun install --frozen-lockfile >/dev/null) && (cd "$here" && bun run build >/dev/null)

mail=(); [ ! -f "$cfg/mail.key" ] || mail=(--var "MAIL_ENDPOINT:$MAIL_ENDPOINT" --var "MAIL_FROM:$MAIL_FROM")
wr deploy --var "PUBLIC_ORIGIN:https://$STATUS_HOST" --var "NET_CHECK_URL:$NET_CHECK_URL" ${mail[@]+"${mail[@]}"} >/dev/null

# Secrets in one bulk call from a private temp file; nothing is echoed.
tmp=$(umask 077; mktemp -d); trap 'rm -rf "$tmp"' EXIT
python3 - "$cfg" >"$tmp/secrets.json" <<'PY'
import glob, json, os, sys
cfg = sys.argv[1]
read = lambda n: open(f"{cfg}/{n}").read().strip()
keys = {os.path.basename(p)[6:-4]: open(p).read().strip() for p in glob.glob(f"{cfg}/probe-*.key")}
out = {"PROBE_KEYS": json.dumps(keys), "ADMIN_TOKEN": read("admin.token"), "CHECKS": read("checks.json")}
if os.path.exists(f"{cfg}/mail.key"):
    out["MAIL_KEY"] = read("mail.key")
print(json.dumps(out))
PY
wr secret bulk "$tmp/secrets.json" >/dev/null

# The custom domain (a Worker custom domain: Cloudflare creates the DNS record itself). Needs the zone id; the token may not be allowed to.
zone=${STATUS_ZONE_ID:-}
if [ -n "$zone" ]; then
  cf -X PUT "$acct/workers/domains" --data "{\"hostname\":\"$STATUS_HOST\",\"service\":\"paperboat-status\",\"environment\":\"production\",\"zone_id\":\"$zone\"}" >/dev/null \
    && echo "attached $STATUS_HOST" || echo "could not attach $STATUS_HOST: Worker paperboat-status > Settings > Domains & Routes > Add > Custom domain" >&2
fi
echo "Done."
