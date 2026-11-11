#!/usr/bin/env python3
"""R-04 staging control plane on anvit: own Compose project, own PostgreSQL, own ports, never prod.

Run from oracle; every remote step is `ssh ubuntu@anvit` (STAGING_HOST). Subcommands:
  build <commit>       build paperboat-server:stg-<short> natively (arm64) on anvit from that commit
  init                 write the stack's files and secrets (kept on rerun); WorkOS ids and the Cloudflare DNS token
                       are read from the production server env over ssh and never printed
  up <image>           start the stack on that image (migrate first), wait for /readyz
  seed                 account, paid entitlement and a console session for the staging test account (idempotent)
  enroll <name>        print one machine enrollment token on stdout only (TOKEN_CMD for tools/probe/canary-setup.sh)
  edge <image>           register and run a staging edge (tunnel container, carrier on TCP 18445) for the staging account
  reset                new empty database; the regional certificate is kept (Let's Encrypt limits duplicates)
  status | down | logs
The API is https://<region>.<regional domain>:<port>, served by the server itself (regional TLS with a Let's
Encrypt wildcard, DNS-01 on the dev zone), the same way a production regional node serves its name.
"""
import base64, datetime, hashlib, json, os, secrets, subprocess, sys, time, urllib.request
from pathlib import Path

HOST = os.environ.get("STAGING_HOST", "ubuntu@anvit")
PROD = os.environ.get("STAGING_PROD_HOST", "root@coolify")
DIR = "pb-release-staging"                     # on HOST, relative to its home
REGION, INSTANCE = "anvit", "slot-01"
# The regional domain is "stg." under the production API host's parent domain (read from the prod server env at init,
# kept in the stack's .env): no domain is written in code.
PORT = int(os.environ.get("STAGING_PORT", "18443"))           # inside anvit's open public range 10000-59999
LOOPBACK = 18091                                              # plain API on anvit's loopback (seed, probes)
ACME = os.environ.get("STAGING_ACME_DIRECTORY", "https://acme-v02.api.letsencrypt.org/directory")
ACCOUNT_REF, DNS_REF = "secret://acme/account", "secret://cloudflare/dns"
# PAPERBOAT_CERT_SECRET_ names are derived from the references (tunnelcert.EnvironmentReferenceName).
ACCOUNT_ENV = "PAPERBOAT_CERT_SECRET_SECRET___ACME_ACCOUNT_27c59c1f5611"
DNS_ENV = "PAPERBOAT_CERT_SECRET_SECRET___CLOUDFLARE_DNS_f9b5f91598d2"
ROOT = Path(__file__).resolve().parent
REPO = ROOT.parent


def read_remote_env(host, path):
    """Read env values only inside the consuming program; never shell-filter multiline secrets."""
    program = ("import json,shlex;from pathlib import Path;"
               f"p=Path({path!r}).expanduser();"
               "items=shlex.split(p.read_text(),comments=True) if p.exists() else [];"
               "print(json.dumps(dict(item.split('=',1) for item in items if '=' in item)))")
    return json.loads(ssh(host, 'python3 -c ' + shell_quote(program)))


def read_remote_secret(host, path):
    program = f"from pathlib import Path;print(Path({path!r}).expanduser().read_text().strip())"
    return ssh(host, 'python3 -c ' + shell_quote(program)).strip()


def fixture_credentials():
    """Random fixture authority stays private on staging; source code contains no usable cookie."""
    program = """import json,os,secrets
from pathlib import Path
p=Path('.fixture-auth.json')
if not p.exists():
    with open(p,'x',opener=lambda path,flags:os.open(path,flags,0o600)) as f:
        json.dump({'session':secrets.token_urlsafe(48),'csrf':secrets.token_urlsafe(48)},f)
if p.stat().st_mode & 0o077:
    raise SystemExit('staging fixture authority must be owner-only')
print(p.read_text())
"""
    return json.loads(remote('python3 -c ' + shell_quote(program)))


def api_url():
    return f"https://{REGION}.{domain()}:{PORT}"


def domain():
    got = os.environ.get("STAGING_DOMAIN") or read_remote_env(HOST, "~/" + DIR + "/.env").get("PAPERBOAT_STAGING_DOMAIN", "")
    if not got:
        raise SystemExit("staging: no staging domain; run `staging.py init` first")
    return got


def ssh(host, script, stdin=None, check=True, tail=300):
    done = subprocess.run(["ssh", "-o", "BatchMode=yes", host, script], input=stdin, capture_output=True, text=True, timeout=900)
    if check and done.returncode:
        raise SystemExit(f"staging: `{script[:80]}` on {host} failed (exit {done.returncode}): {done.stderr.strip()[-tail:]}")
    return done.stdout


def remote(script, **kw):
    return ssh(HOST, f"cd ~/{DIR} && {script}", **kw)


def new_acme_key():
    ec = subprocess.run(["openssl", "ecparam", "-genkey", "-name", "prime256v1"], capture_output=True, check=True).stdout
    return subprocess.run(["openssl", "pkcs8", "-topk8", "-nocrypt"], input=ec, capture_output=True, check=True).stdout.decode()


def build_env(existing, prod):
    """The staging server.env: existing random secrets are kept, production credentials are copied in memory."""
    keep = lambda k, make: existing.get(k) or make()
    api, DOMAIN = api_url(), domain()
    sub = lambda name: f"{name}.{DOMAIN}"
    workos = lambda f: read_remote_secret(PROD, f"/opt/paperboat/secrets/{f}")
    env = {
        "PAPERBOAT_ENV": "development", "PAPERBOAT_DATABASE_DRIVER": "postgres",
        "PAPERBOAT_DATABASE_DSN": f"postgres://postgres:{keep_password(existing)}@postgres:5432/paperboat_staging?sslmode=disable",
        "PAPERBOAT_HTTP_ADDRESS": "0.0.0.0:8080", "PAPERBOAT_PUBLIC_BASE_URL": api,
        "PAPERBOAT_OPERATOR_EMAIL": prod.get("PAPERBOAT_OPERATOR_EMAIL", ""),
        "PAPERBOAT_WORKOS_CLIENT_ID": existing.get("PAPERBOAT_WORKOS_CLIENT_ID") or workos("paperboat_workos_client_id"),
        "PAPERBOAT_WORKOS_API_KEY": existing.get("PAPERBOAT_WORKOS_API_KEY") or workos("paperboat_workos_api_key"),
        "PAPERBOAT_SESSION_KEYS": keep("PAPERBOAT_SESSION_KEYS", lambda: secrets.token_urlsafe(48)),
        "PAPERBOAT_ENCRYPTION_KEY": keep("PAPERBOAT_ENCRYPTION_KEY", lambda: secrets.token_urlsafe(48)),
        # without fixed mint keys every restart invalidates every machine identity (401 machine_identity_invalid)
        "PAPERBOAT_MINT_SIGNING_KEYS": keep("PAPERBOAT_MINT_SIGNING_KEYS", lambda: "staging1:" + secrets.token_urlsafe(32)),
        "PAPERBOAT_MINT_ACTIVE_KEY_ID": "staging1",
        # mounts the edge control endpoints (/v1/edge/*, /v1/nodes/*); without it every edge gets 404
        "PAPERBOAT_EDGE_CONTROL_CREDENTIAL": keep("PAPERBOAT_EDGE_CONTROL_CREDENTIAL", lambda: secrets.token_urlsafe(48)),
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
    env = read_remote_env(HOST, "~/" + DIR + "/server.env")
    pw = read_remote_secret(HOST, "~/" + DIR + "/secrets/postgres_password") if env else ""
    if pw:
        env["_postgres_password"] = pw
    return env


def init():
    existing = read_existing()
    prod = read_remote_env(PROD, "/opt/paperboat/server.env")
    parent = prod["PAPERBOAT_PUBLIC_BASE_URL"].split("://", 1)[1].split("/")[0].split(":")[0].split(".", 1)[1]
    os.environ.setdefault("STAGING_DOMAIN", "stg." + parent)
    prod = {k: v for k, v in prod.items() if k in ("PAPERBOAT_OPERATOR_EMAIL", "PAPERBOAT_CERTIFICATES_DNS_ZONE_ID", DNS_ENV)}
    password = keep_password(existing)
    existing["_postgres_password"] = password
    ssh(HOST, f"mkdir -p ~/{DIR}/secrets ~/{DIR}/releases && chmod 700 ~/{DIR} ~/{DIR}/secrets")
    remote("umask 077 && cat > server.env", stdin=build_env(existing, prod))
    remote("umask 077 && cat > secrets/postgres_password", stdin=password)
    remote("cat > compose.yaml", stdin=(ROOT / "staging.compose.yaml").read_text())
    remote("umask 077 && cat > .env", stdin=f"PAPERBOAT_STAGING_PROJECT=pb-release-staging\nPAPERBOAT_POSTGRES_IMAGE=postgres:17\n"
           f"PAPERBOAT_STAGING_API_PORT={LOOPBACK}\nPAPERBOAT_STAGING_HTTPS_PORT={PORT}\nPAPERBOAT_STAGING_DOMAIN={os.environ['STAGING_DOMAIN']}\n"
           + (f"PAPERBOAT_SERVER_IMAGE={existing_image()}\n"))
    print(json.dumps({"ok": True, "api": api_url(), "dns": ensure_dns(prod)}))


def ensure_dns(prod):
    """One A record <region>.<domain> -> anvit's public address (a lone node has no enrolled addresses for the DNS worker)."""
    address = ssh(HOST, "curl -s -m10 https://api.ipify.org").strip()
    zone, name = prod["PAPERBOAT_CERTIFICATES_DNS_ZONE_ID"], f"{REGION}.{domain()}"
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
    out = read_remote_env(HOST, "~/" + DIR + "/.env").get("PAPERBOAT_SERVER_IMAGE", "")
    return out or "paperboat-server:unset"


def source_archive(commit):
    """(short, full, tar) of the image's source: the repository root of that commit, without the console, marketing and tools
    (README 'Building and deploying images'). git archive's pathspec is relative to the directory it runs in, so it runs at the root."""
    git = lambda *args: subprocess.run(["git", *args], capture_output=True, check=True, cwd=REPO).stdout
    full = git("rev-parse", commit).decode().strip()
    short = git("rev-parse", "--short", full).decode().strip()
    return short, full, git("archive", full, "--", ".", ":!console", ":!marketing", ":!tools")


def build(commit):
    short, full, tar = source_archive(commit)
    src = f"pb-stg-src-{short}"
    done = subprocess.run(["ssh", "-o", "BatchMode=yes", HOST, f"rm -rf ~/{src} && mkdir ~/{src} && tar x -C ~/{src}"], input=tar, capture_output=True, timeout=300)
    if done.returncode:
        raise SystemExit(f"staging: copying the source to {HOST} failed: {done.stderr.decode()[-300:]}")
    image = f"paperboat-server:stg-{short}"
    # The Dockerfile accepts only YYYY.MM.DD.N: a staging build is today's date with a high N derived from the commit, never a release number.
    version = datetime.datetime.now(datetime.timezone.utc).strftime("%Y.%m.%d") + f".{900000 + int(full[:5], 16) % 100000}"
    # The build log stays on the host (~/pb-stg-build.log); a failure prints its last lines: the compiler error, not just 'exit code 1'.
    ssh(HOST, f"cd ~/{src} && nice -n 10 docker build --progress=plain --build-arg PAPERBOAT_VERSION={version} --build-arg PAPERBOAT_COMMIT={full} "
              f"-t {image} -f server/deploy/Dockerfile . > ~/pb-stg-build.log 2>&1; rc=$?; rm -rf ~/{src}; "
              f"[ $rc = 0 ] || {{ grep -v -E '^#[0-9]+ (DONE|CACHED|sha256)' ~/pb-stg-build.log | tail -25 >&2; }}; exit $rc", tail=2500)
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
            "api": api_url(), "image": existing_image()}


def sql(statement):
    return remote("docker compose exec -T -e PGOPTIONS=-csearch_path=paperboat,public postgres psql -U postgres -d paperboat_staging -v ON_ERROR_STOP=1 -Atc " + shell_quote(statement))


def shell_quote(text):
    return "'" + text.replace("'", "'\\''") + "'"


def seed():
    authority = fixture_credentials()
    session, csrf = authority["session"], authority["csrf"]
    sha = lambda v: subprocess.run(["sha256sum"], input=v.encode(), capture_output=True, check=True).stdout.split()[0].decode()
    sql("INSERT INTO paperboat.users(id,workos_subject,primary_email,status) VALUES('usr_staging','workos_staging','staging@example.test','active') ON CONFLICT DO NOTHING")
    sql("INSERT INTO paperboat.user_machine_entitlements (id,user_id,provider_subscription_id,product_code,state,seat_quantity,allowance_bytes,current_period_start,current_period_end) "
        "VALUES ('ume_staging','usr_staging','sub_staging','connected-test','active',64,1099511627776,now()-interval '1 hour',now()+interval '30 days') "
        "ON CONFLICT (id) DO UPDATE SET current_period_start=excluded.current_period_start, current_period_end=excluded.current_period_end, state='active'")
    sql("INSERT INTO paperboat.sessions(id,user_id,session_hash,csrf_hash,expires_at,workos_session_id,workos_access_expires_at) "
        f"VALUES('session_staging','usr_staging','{sha(session)}','{sha(csrf)}',now()+interval '30 days','session_staging',now()+interval '30 days') "
        "ON CONFLICT (id) DO UPDATE SET session_hash=excluded.session_hash, csrf_hash=excluded.csrf_hash, expires_at=excluded.expires_at, workos_access_expires_at=excluded.workos_access_expires_at")
    print(json.dumps({"ok": True, "account": "usr_staging"}))


def enroll(name):
    fixture_credentials()
    # Read cookies inside the remote process; never put credentials in curl/ssh argv.
    program = f"""import json,secrets,urllib.request,urllib.error
from pathlib import Path
c=json.loads(Path('.fixture-auth.json').read_text())
request=urllib.request.Request('http://127.0.0.1:{LOOPBACK}/v1/machine-enrollments',data=b'{{}}',headers={{
 'Cookie':'__Host-pb-session='+c['session']+'; __Host-pb-csrf='+c['csrf'],
 'X-CSRF-Token':c['csrf'],'Origin':{api_url()!r},'Content-Type':'application/json',
 'Idempotency-Key':'staging-'+secrets.token_hex(16)}})
try:
 with urllib.request.urlopen(request,timeout=20) as response: print(response.read().decode())
except urllib.error.HTTPError as error:
 print(json.dumps({{'http_status':error.code}}))
"""
    out = remote('python3 -c ' + shell_quote(program))
    try:
        data = json.loads(out)["data"]["bootstrap_token"]
    except (ValueError, KeyError, TypeError):
        raise SystemExit(f"staging: enrollment for {name} was refused (run `staging.py seed` first)")
    sys.stdout.write(data)


EDGE_QUIC_PORT = 18448  # UDP is closed on the host; the config still needs a distinct QUIC port
EDGE_PORT = 18445   # carrier TCP (inside anvit's open 10000-59999 range); the prod edge keeps 80/443/27443-27446
EDGE_NAME, EDGE_DIR = "pb-stg-edge", "edge"


def edge(image):
    """Account-scoped self-hosted edge for usr_staging, as plain `docker run` (no pbh, no host firewall or nft change).
    Registration mirrors server/internal/controlplane/selfhost_tunnel_process_test.go: what the claim flow would store.
    Limits: no browser access (preview URLs never carry a port, so previews need 443) and no HTTP/3 (UDP is closed)."""
    from cryptography import x509
    from cryptography.hazmat.primitives import hashes, serialization
    from cryptography.hazmat.primitives.asymmetric import ed25519
    from cryptography.x509.oid import NameOID
    host = f"{REGION}.{domain()}"
    api = api_url()
    b64 = lambda raw: base64.urlsafe_b64encode(raw).decode().rstrip("=")
    seed = ed25519.Ed25519PrivateKey.generate()
    raw_seed = seed.private_bytes(serialization.Encoding.Raw, serialization.PrivateFormat.Raw, serialization.NoEncryption())
    raw_pub = seed.public_key().public_bytes(serialization.Encoding.Raw, serialization.PublicFormat.Raw)
    key = ed25519.Ed25519PrivateKey.generate()   # the carrier certificate must be Ed25519
    name = x509.Name([x509.NameAttribute(NameOID.COMMON_NAME, host)])
    now = datetime.datetime.now(datetime.timezone.utc)
    cert = (x509.CertificateBuilder().subject_name(name).issuer_name(name).public_key(key.public_key()).serial_number(x509.random_serial_number())
            .not_valid_before(now - datetime.timedelta(minutes=5)).not_valid_after(now + datetime.timedelta(days=365))
            .add_extension(x509.SubjectAlternativeName([x509.DNSName(host)]), critical=False).sign(key, None))
    pin = hashlib.sha256(cert.public_key().public_bytes(serialization.Encoding.DER, serialization.PublicFormat.SubjectPublicKeyInfo)).hexdigest()
    suffix = secrets.token_hex(6)
    install, node, credential, usage = f"stg_install_{suffix}", f"stg_node_{suffix}", secrets.token_urlsafe(32), f"stg_usage_{suffix}"
    jwks = remote(f"curl -sf -m10 http://127.0.0.1:{LOOPBACK}/.well-known/jwks.json")
    files = {
        "runtime.credential": credential, "jwks.json": jwks,
        "revocations.json": json.dumps({"jtis": [], "environments": [], "connector_generations": [], "key_ids": []}),
        "usage.key": json.dumps({"key_id": usage, "edge_node_id": node, "private_key": b64(raw_seed + raw_pub)}),
        "tls.crt": cert.public_bytes(serialization.Encoding.PEM).decode(),
        "tls.key": key.private_bytes(serialization.Encoding.PEM, serialization.PrivateFormat.PKCS8, serialization.NoEncryption()).decode(),
        "deployment.json": json.dumps({
            "self_hosted": True, "browser_access_enabled": False, "browser_login_origin": "", "control_url": api, "credential_issuer": api,
            "control_credential_file": "/state/runtime.credential", "control_ca_file": "", "jwks_file": "/state/jwks.json",
            "revocations_file": "/state/revocations.json", "usage_signing_key_file": "/state/usage.key",
            "infrastructure_tls_cert_file": "/state/tls.crt", "infrastructure_tls_key_file": "/state/tls.key",
            "connector_advertise_host": host, "carrier_tcp_listen_address": f"0.0.0.0:{EDGE_PORT}",
            "carrier_quic_listen_address": f"0.0.0.0:{EDGE_QUIC_PORT}", "public_https_listen_address": "0.0.0.0:18446",
            "private_https_listen_address": "127.0.0.1:9443", "public_http_listen_address": "0.0.0.0:18447",
            "preview_base_domain": f"prev.{domain()}", "tunnel_base_domain": f"tun.{domain()}", "runtime_base_domain": f"rt.{domain()}",
            "trusted_proxy_cidrs": [], "public_routes": [], "node_capacity": 32, "control_interval": 5_000_000_000,
            "control_timeout": 5_000_000_000, "max_body_bytes": 52428800, "max_header_bytes": 32768}),
    }
    remote(f"docker rm -f {EDGE_NAME} >/dev/null 2>&1; rm -rf {EDGE_DIR}; mkdir -m 700 {EDGE_DIR}")
    for filename, content in files.items():
        remote(f"umask 077 && cat > {EDGE_DIR}/{filename}", stdin=content)
    from cryptography.hazmat.primitives.ciphers.aead import AESGCM
    sealing = read_remote_env(HOST, "~/" + DIR + "/server.env")["PAPERBOAT_ENCRYPTION_KEY"]   # secrets.Encrypt: AES-256-GCM, key = sha256(key)
    nonce = os.urandom(12)
    sealed = nonce + AESGCM(hashlib.sha256(sealing.encode()).digest()).encrypt(nonce, credential.encode(), None)
    sql("DELETE FROM paperboat.selfhost_installations WHERE scope_id='usr_staging'")
    sql("DELETE FROM paperboat.control_tunnel_nodes WHERE id LIKE 'stg_node_%'")
    sql(f"INSERT INTO paperboat.control_tunnel_nodes(id,edge_pool,protocol_version,process_epoch,endpoint_host,endpoint_tcp_port,endpoint_quic_port,state,region,failure_domain,"
        f"roles,transports,capacity_limit,capacity_observed_at,registry_expires_at,tls_spki_sha256) VALUES('{node}','{install}','1.0','initial_fixture','{host}',{EDGE_PORT},{EDGE_QUIC_PORT},"
        f"'registered','{REGION}','{REGION}',ARRAY['edge'],ARRAY['http2','http3'],32,now(),now()+interval '1 hour',decode('{pin}','hex'))")
    sql(f"INSERT INTO paperboat.selfhost_installations(id,node_id,installation_public_key,runtime_hash,runtime_ciphertext,name,capability,scope_kind,scope_id,claim_id,claim_complete) "
        f"VALUES('{install}','{node}',decode('{raw_pub.hex()}','hex'),decode('{hashlib.sha256(credential.encode()).hexdigest()}','hex'),decode('{sealed.hex()}','hex'),"
        f"'{EDGE_NAME}','tunnel','account','usr_staging','{install}',true)")
    # Staging cannot pass the 443 ingress probe, so its edge is marked verified by hand, and the account is pinned to its own pool
    # (a self-hosted edge serves only accounts whose pool policy is self-hosted-only).
    sql(f"UPDATE paperboat.control_tunnel_nodes SET public_ingress_verified_at=now() WHERE id='{node}'")
    sql(f"INSERT INTO paperboat.selfhost_pool_policies(scope_kind,scope_id,capability,mode,installation_ids) VALUES('account','usr_staging','tunnel','self-hosted-only',ARRAY['{install}']) "
        "ON CONFLICT (scope_kind,scope_id,capability) DO UPDATE SET mode=excluded.mode, installation_ids=excluded.installation_ids")
    sql(f"INSERT INTO paperboat.control_usage_verification_keys(key_id,edge_node_id,public_key,not_before,expires_at) VALUES('{usage}','{node}',decode('{raw_pub.hex()}','hex'),now()-interval '1 minute',now()+interval '365 days')")
    args = f"-node-id {node} -edge-pool {install} -relay-id {node} -relay-region {REGION} -relay-name {EDGE_NAME} -health-address 127.0.0.1:9090 -deployment-config /state/deployment.json -state-path /state/state.json"
    remote(f"docker run -d --name {EDGE_NAME} --restart unless-stopped --user 0:0 -v $PWD/{EDGE_DIR}:/state -p {EDGE_PORT}:{EDGE_PORT} -p {EDGE_QUIC_PORT}:{EDGE_QUIC_PORT}/udp "
           f"-p 18446:18446 -p 18447:18447 {image} {args}")
    return {"node": node, "installation": install}


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
            "status": (lambda: print(json.dumps(status())), 0), "edge": (lambda image: print(json.dumps(edge(image))), 1), "down": (lambda: remote("docker compose down"), 0),
            "logs": (lambda: print(remote("docker compose logs --no-color --tail 80 server")), 0)}

if __name__ == "__main__":
    if len(sys.argv) < 2 or sys.argv[1] not in COMMANDS or len(sys.argv) - 2 != COMMANDS[sys.argv[1]][1]:
        sys.exit(__doc__)
    COMMANDS[sys.argv[1]][0](*sys.argv[2:])
