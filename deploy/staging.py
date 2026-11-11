#!/usr/bin/env python3
"""R-04 staging control plane on anvit: own Compose project, own PostgreSQL, own ports, never prod.

Run from oracle; every remote step is `ssh ubuntu@anvit` (STAGING_HOST). Subcommands:
  build <commit>       build paperboat-server:stg-<short> natively (arm64) on anvit from that commit
  init                 write the stack's files and secrets (kept on rerun); WorkOS ids and the Cloudflare DNS token
                       are read from the production server env over ssh and never printed
  up <image>           start the stack on that image (migrate first), wait for /readyz
  seed                 account, paid entitlement and a console session for the staging test account (idempotent)
  enroll <name>        print one machine enrollment token on stdout only (TOKEN_CMD for tools/probe/canary-setup.sh)
  reset                new empty database; the regional certificate is kept (Let's Encrypt limits duplicates)
  status | down | logs
The API is https://<region>.<regional domain>:<port>, served by the server itself (regional TLS with a Let's
Encrypt wildcard, DNS-01 on the dev zone), the same way a production regional node serves its name.
"""
import json, os, secrets, subprocess, sys, time, urllib.request
from pathlib import Path

HOST = os.environ.get("STAGING_HOST", "ubuntu@anvit")
PROD = os.environ.get("STAGING_PROD_HOST", "root@coolify")
DIR = "pb-release-staging"                     # on HOST, relative to its home
REGION, INSTANCE = "anvit", "slot-01"
DOMAIN = os.environ.get("STAGING_DOMAIN", "stg.pprbt.dev")   # regional domain; zone is its parent
PORT = int(os.environ.get("STAGING_PORT", "18443"))           # inside anvit's open public range 10000-59999
LOOPBACK = 18091                                              # plain API on anvit's loopback (seed, probes)
ACME = os.environ.get("STAGING_ACME_DIRECTORY", "https://acme-v02.api.letsencrypt.org/directory")
ACCOUNT_REF, DNS_REF = "secret://acme/account", "secret://cloudflare/dns"
# PAPERBOAT_CERT_SECRET_ names are derived from the references (tunnelcert.EnvironmentReferenceName).
ACCOUNT_ENV = "PAPERBOAT_CERT_SECRET_SECRET___ACME_ACCOUNT_27c59c1f5611"
DNS_ENV = "PAPERBOAT_CERT_SECRET_SECRET___CLOUDFLARE_DNS_f9b5f91598d2"
ROOT = Path(__file__).resolve().parent


def ssh(host, script, stdin=None, check=True):
    done = subprocess.run(["ssh", "-o", "BatchMode=yes", host, script], input=stdin, capture_output=True, text=True, timeout=900)
    if check and done.returncode:
        raise SystemExit(f"staging: `{script[:80]}` on {host} failed (exit {done.returncode}): {done.stderr.strip()[-300:]}")
    return done.stdout


def remote(script, **kw):
    return ssh(HOST, f"cd ~/{DIR} && {script}", **kw)


def parse_env(text):
    """KEY=value lines; a single-quoted value may span lines (the ACME key)."""
    out, key, buf = {}, None, []
    for line in text.splitlines():
        if key is None:
            if "=" in line and not line.startswith("#"):
                k, v = line.split("=", 1)
                if v.startswith("'") and not (len(v) > 1 and v.endswith("'")):
                    key, buf = k, [v[1:]]
                else:
                    out[k] = v.strip("'")
        else:
            buf.append(line.rstrip("'") if line.endswith("'") else line)
            if line.endswith("'"):
                out[key], key = "\n".join(buf), None
    return out


def new_acme_key():
    ec = subprocess.run(["openssl", "ecparam", "-genkey", "-name", "prime256v1"], capture_output=True, check=True).stdout
    return subprocess.run(["openssl", "pkcs8", "-topk8", "-nocrypt"], input=ec, capture_output=True, check=True).stdout.decode()


def build_env(existing, prod):
    """The staging server.env: existing random secrets are kept, production credentials are copied in memory."""
    keep = lambda k, make: existing.get(k) or make()
    api = f"https://{REGION}.{DOMAIN}:{PORT}"
    sub = lambda name: f"{name}.{DOMAIN}"
    workos = lambda f: ssh(PROD, f"cat /opt/paperboat/secrets/{f}").strip()
    env = {
        "PAPERBOAT_ENV": "development", "PAPERBOAT_DATABASE_DRIVER": "postgres",
        "PAPERBOAT_DATABASE_DSN": f"postgres://postgres:{keep_password(existing)}@postgres:5432/paperboat_staging?sslmode=disable",
        "PAPERBOAT_HTTP_ADDRESS": "0.0.0.0:8080", "PAPERBOAT_PUBLIC_BASE_URL": api,
        "PAPERBOAT_OPERATOR_EMAIL": prod.get("PAPERBOAT_OPERATOR_EMAIL", ""),
        "PAPERBOAT_WORKOS_CLIENT_ID": existing.get("PAPERBOAT_WORKOS_CLIENT_ID") or workos("paperboat_workos_client_id"),
        "PAPERBOAT_WORKOS_API_KEY": existing.get("PAPERBOAT_WORKOS_API_KEY") or workos("paperboat_workos_api_key"),
        "PAPERBOAT_SESSION_KEYS": keep("PAPERBOAT_SESSION_KEYS", lambda: secrets.token_urlsafe(48)),
        "PAPERBOAT_ENCRYPTION_KEY": keep("PAPERBOAT_ENCRYPTION_KEY", lambda: secrets.token_urlsafe(48)),
        "PAPERBOAT_RELEASE_DIRECTORY": "/srv/paperboat-releases", "PAPERBOAT_RELEASE_BASE_URL": api,
        "PAPERBOAT_CLI_VERIFICATION_URL": api + "/cli", "PAPERBOAT_MACHINES_URL": api + "/machines",
        "PAPERBOAT_BROWSER_LOGIN_ORIGIN": api,
        "PAPERBOAT_LOCAL_ACCESS_BASE_DOMAIN": sub("loc"), "PAPERBOAT_PREVIEW_BASE_DOMAIN": sub("prev"),
        "PAPERBOAT_TUNNEL_BASE_DOMAIN": sub("tun"), "PAPERBOAT_RUNTIME_BASE_DOMAIN": sub("rt"),
        "PAPERBOAT_SELFHOST_ALLOW_PRIVATE_INGRESS": "true",
        "PAPERBOAT_SENTRY_REGION": REGION, "PAPERBOAT_SENTRY_INSTANCE": INSTANCE,
        "PAPERBOAT_REGIONAL_DOMAIN": DOMAIN, "PAPERBOAT_REGIONAL_DNS_ZONE_ID": prod["PAPERBOAT_CERTIFICATES_DNS_ZONE_ID"],
        "PAPERBOAT_REGIONAL_HTTPS_ADDRESS": f":{PORT}",
        "PAPERBOAT_CERTIFICATES_DIRECTORY_URL": ACME, "PAPERBOAT_CERTIFICATES_ACCOUNT_KEY_REFERENCE": ACCOUNT_REF,
        "PAPERBOAT_CERTIFICATES_DNS_TOKEN_REFERENCE": DNS_REF,
        DNS_ENV: existing.get(DNS_ENV) or prod[DNS_ENV],
        ACCOUNT_ENV: existing.get(ACCOUNT_ENV) or new_acme_key(),
    }
    return "".join(f"{k}='{v}'\n" if "\n" in v else f"{k}={v}\n" for k, v in env.items())


def keep_password(existing):
    return existing.get("_postgres_password") or secrets.token_hex(24)


def read_existing():
    text = remote("cat server.env 2>/dev/null", check=False)
    env = parse_env(text)
    pw = remote("cat secrets/postgres_password 2>/dev/null", check=False).strip()
    if pw:
        env["_postgres_password"] = pw
    return env


def init():
    existing = read_existing()
    prod = parse_env(ssh(PROD, "cat /opt/paperboat/server.env"))
    prod = {k: v for k, v in prod.items() if k in ("PAPERBOAT_OPERATOR_EMAIL", "PAPERBOAT_CERTIFICATES_DNS_ZONE_ID", DNS_ENV)}
    password = keep_password(existing)
    existing["_postgres_password"] = password
    ssh(HOST, f"mkdir -p ~/{DIR}/secrets ~/{DIR}/releases && chmod 700 ~/{DIR} ~/{DIR}/secrets")
    remote("umask 077 && cat > server.env", stdin=build_env(existing, prod))
    remote("umask 077 && cat > secrets/postgres_password", stdin=password)
    remote("cat > compose.yaml", stdin=(ROOT / "staging.compose.yaml").read_text())
    remote("umask 077 && cat > .env", stdin=f"PAPERBOAT_STAGING_PROJECT=pb-release-staging\nPAPERBOAT_POSTGRES_IMAGE=postgres:17\n"
           f"PAPERBOAT_STAGING_API_PORT={LOOPBACK}\nPAPERBOAT_STAGING_HTTPS_PORT={PORT}\n"
           + (f"PAPERBOAT_SERVER_IMAGE={existing_image()}\n"))
    print(json.dumps({"ok": True, "api": f"https://{REGION}.{DOMAIN}:{PORT}", "dns": ensure_dns(prod)}))


def ensure_dns(prod):
    """One A record <region>.<domain> -> anvit's public address (a lone node has no enrolled addresses for the DNS worker)."""
    address = ssh(HOST, "curl -s -m10 https://api.ipify.org").strip()
    zone, name = prod["PAPERBOAT_CERTIFICATES_DNS_ZONE_ID"], f"{REGION}.{DOMAIN}"
    def api(method, path, body=None):
        request = urllib.request.Request("https://api.cloudflare.com/client/v4" + path, method=method, headers={
            "Authorization": "Bearer " + prod[DNS_ENV], "Content-Type": "application/json"}, data=json.dumps(body).encode() if body else None)
        return json.load(urllib.request.urlopen(request, timeout=20))["result"]
    have = api("GET", f"/zones/{zone}/dns_records?type=A&name={name}")
    if have and have[0]["content"] == address:
        return f"{name} {address}"
    record = {"type": "A", "name": name, "content": address, "ttl": 60, "proxied": False}
    api("PUT", f"/zones/{zone}/dns_records/{have[0]['id']}", record) if have else api("POST", f"/zones/{zone}/dns_records", record)
    return f"{name} {address}"


def existing_image():
    out = remote("grep '^PAPERBOAT_SERVER_IMAGE=' .env 2>/dev/null | cut -d= -f2", check=False).strip()
    return out or "paperboat-server:unset"


def build(commit):
    short = subprocess.run(["git", "rev-parse", "--short", commit], capture_output=True, text=True, check=True, cwd=ROOT).stdout.strip()
    full = subprocess.run(["git", "rev-parse", commit], capture_output=True, text=True, check=True, cwd=ROOT).stdout.strip()
    tar = subprocess.run(["git", "archive", full, "--", ".", ":!console", ":!marketing", ":!tools"], capture_output=True, check=True, cwd=ROOT).stdout
    src = f"pb-stg-src-{short}"
    done = subprocess.run(["ssh", "-o", "BatchMode=yes", HOST, f"rm -rf ~/{src} && mkdir ~/{src} && tar x -C ~/{src}"], input=tar, capture_output=True, timeout=300)
    if done.returncode:
        raise SystemExit(f"staging: copying the source to {HOST} failed: {done.stderr.decode()[-300:]}")
    image = f"paperboat-server:stg-{short}"
    ssh(HOST, f"cd ~/{src} && nice -n 10 docker build -q --build-arg PAPERBOAT_VERSION=stg-{short} --build-arg PAPERBOAT_COMMIT={full} "
              f"-t {image} -f server/deploy/Dockerfile . >/dev/null; rc=$?; rm -rf ~/{src}; exit $rc")
    print(image)


def up(image):
    remote(f"sed -i 's|^PAPERBOAT_SERVER_IMAGE=.*|PAPERBOAT_SERVER_IMAGE={image}|' .env && docker compose up -d --remove-orphans")
    wait_ready()
    print(json.dumps(status()))


def wait_ready(seconds=180):
    end = time.monotonic() + seconds
    while time.monotonic() < end:
        if remote(f"curl -s -m3 -o /dev/null -w '%{{http_code}}' http://127.0.0.1:{LOOPBACK}/readyz", check=False).strip() == "200":
            return
        time.sleep(3)
    raise SystemExit("staging: the server did not answer /readyz within 3 minutes; run `staging.py logs`")


def status():
    version = remote(f"curl -s -m5 http://127.0.0.1:{LOOPBACK}/v1/server-version", check=False).strip()
    cert = remote("docker compose exec -T postgres psql -U postgres -d paperboat_staging -Atc "
                  "\"select count(*) from paperboat.control_server_node_acme_cache where key='regional-wildcard'\"", check=False).strip()
    return {"server_version": version[:200], "regional_certificate_stored": cert == "1",
            "api": f"https://{REGION}.{DOMAIN}:{PORT}", "image": existing_image()}


def sql(statement):
    return remote("docker compose exec -T postgres psql -U postgres -d paperboat_staging -v ON_ERROR_STOP=1 -Atc " + shell_quote(statement))


def shell_quote(text):
    return "'" + text.replace("'", "'\\''") + "'"


SESSION, CSRF = "staging-session", "staging-csrf"


def seed():
    sha = lambda v: subprocess.run(["sha256sum"], input=v.encode(), capture_output=True, check=True).stdout.split()[0].decode()
    sql("INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES('usr_staging','workos_staging','staging@example.test','active') ON CONFLICT DO NOTHING")
    sql("INSERT INTO paperboat.user_machine_entitlements (id,user_id,provider_subscription_id,product_code,state,seat_quantity,allowance_bytes,current_period_start,current_period_end) "
        "VALUES ('ume_staging','usr_staging','sub_staging','connected-test','active',64,1099511627776,now()-interval '1 hour',now()+interval '30 days') "
        "ON CONFLICT (id) DO UPDATE SET current_period_start=excluded.current_period_start, current_period_end=excluded.current_period_end, state='active'")
    sql("INSERT INTO paperboat.sessions(id,user_id,session_hash,csrf_hash,expires_at,workos_session_id,workos_access_expires_at) "
        f"VALUES('session_staging','usr_staging','{sha(SESSION)}','{sha(CSRF)}',now()+interval '30 days','session_staging',now()+interval '30 days') "
        "ON CONFLICT (id) DO UPDATE SET expires_at=excluded.expires_at, workos_access_expires_at=excluded.workos_access_expires_at")
    print(json.dumps({"ok": True, "account": "usr_staging"}))


def enroll(name):
    base = f"https://{REGION}.{DOMAIN}:{PORT}"
    body = json.dumps({})
    out = remote(f"curl -s -m20 -X POST http://127.0.0.1:{LOOPBACK}/v1/machine-enrollments "
                 f"-H 'Cookie: __Host-pb-session={SESSION}; __Host-pb-csrf={CSRF}' -H 'X-CSRF-Token: {CSRF}' "
                 f"-H 'Origin: {base}' -H 'Content-Type: application/json' -H 'Idempotency-Key: staging-{name}-{secrets.token_hex(8)}' -d '{body}'")
    try:
        data = json.loads(out)["data"]["bootstrap_token"]
    except (ValueError, KeyError, TypeError):
        raise SystemExit(f"staging: enrollment for {name} was refused (run `staging.py seed` first): {out[:200]}")
    sys.stdout.write(data)


def reset():
    remote("docker compose stop server")
    keep = "/tmp/pb-staging-acme-cache.sql"
    remote(f"docker compose exec -T postgres pg_dump -U postgres -d paperboat_staging --data-only -t paperboat.control_server_node_acme_cache > {keep}", check=False)
    remote("docker compose exec -T postgres psql -U postgres -c 'DROP DATABASE IF EXISTS paperboat_staging WITH (FORCE)' -c 'CREATE DATABASE paperboat_staging'")
    remote("docker compose run --rm migrate")
    remote(f"test -s {keep} && docker compose exec -T postgres psql -U postgres -d paperboat_staging -v ON_ERROR_STOP=1 < {keep}; rm -f {keep}", check=False)
    remote("docker compose up -d server")
    wait_ready()
    seed()


COMMANDS = {"init": (init, 0), "build": (build, 1), "up": (up, 1), "seed": (seed, 0), "enroll": (enroll, 1), "reset": (reset, 0),
            "status": (lambda: print(json.dumps(status())), 0), "down": (lambda: remote("docker compose down"), 0),
            "logs": (lambda: print(remote("docker compose logs --no-color --tail 80 server")), 0)}

if __name__ == "__main__":
    if len(sys.argv) < 2 or sys.argv[1] not in COMMANDS or len(sys.argv) - 2 != COMMANDS[sys.argv[1]][1]:
        sys.exit(__doc__)
    COMMANDS[sys.argv[1]][0](*sys.argv[2:])
