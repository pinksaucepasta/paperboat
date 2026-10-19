#!/bin/sh
# Fresh-host qualification of the one-liner installer (S4-02/S4-04) in a throwaway systemd
# container: install, health, idempotent re-run, uninstall, reinstall, data removal.
#   qualify-fresh-host.sh --base ubuntu:24.04 --enable relay|tunnel|both [--platform linux/amd64]
#   qualify-fresh-host.sh --base ubuntu:20.04 --expect-refusal
#   qualify-fresh-host.sh --base ubuntu:24.04 --enable both --published https://<get host>/selfhost/install
#     (the real published one-liner and its auto-detected address instead of the local origin)
# Needs docker (set DOCKER="sudo docker" if required), PAPERBOAT_SELFHOST_RELAY_IMAGE and
# PAPERBOAT_SELFHOST_TUNNEL_IMAGE (published images). Everything it creates is removed on exit.
set -eu
base=ubuntu:24.04 enable=relay platform='' refusal=false published=''
while [ "$#" -gt 0 ]; do
 case "$1" in
  --base) base=$2; shift 2;; --enable) enable=$2; shift 2;; --platform) platform=$2; shift 2;;
  --published) published=$2; shift 2;; --expect-refusal) refusal=true; shift;; *) echo "unknown argument: $1" >&2; exit 2;;
 esac
done
docker=${DOCKER:-docker}
[ -n "$published" ] || : "${PAPERBOAT_SELFHOST_RELAY_IMAGE:?set the published relay image}" "${PAPERBOAT_SELFHOST_TUNNEL_IMAGE:?set the published tunnel image}"
repo=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
work=$(mktemp -d "$repo/.build/qualify.XXXXXX" 2>/dev/null || { mkdir -p "$repo/.build" && mktemp -d "$repo/.build/qualify.XXXXXX"; })
id=$(basename "$work" | tr 'A-Z.' 'a-z-')
name=pbh-qualify-$id
step() { printf '== %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
cleanup() { fresh_host_down; rm -rf "$work"; }
trap cleanup EXIT HUP INT TERM
. "$repo/tools/fresh-host.sh"

arch=$(${docker} run --rm ${platform:+--platform "$platform"} "$base" uname -m)
case "$arch" in x86_64) arch=amd64;; aarch64) arch=arm64;; *) fail "unexpected architecture $arch";; esac
if [ -z "$published" ]; then
step "build the $arch package and installer"
export PAPERBOAT_SELFHOST_ADDRESS_URL=https://address.example.test/selfhost/address
version=0.0.0.1
"$repo/helper/deploy/package-selfhost.sh" "$version" "$arch" "$work/site" >/dev/null
other=amd64; [ "$arch" = amd64 ] && other=arm64
# The installer needs both digests; the other architecture is never downloaded here.
cp "$work/site/paperboat-selfhost-linux-$arch.tar.gz" "$work/site/paperboat-selfhost-linux-$other.tar.gz"
(cd "$work/site" && sha256sum paperboat-selfhost-linux-*.tar.gz > SHA256SUMS)
"$repo/helper/deploy/render-installer.sh" "$version" "https://pkg.example.test:9443/selfhost/versions/$version" https://get.docker.com "$work/site" >/dev/null

step "create the throwaway HTTPS origin and the fresh $base host"
openssl req -x509 -newkey rsa:2048 -nodes -keyout "$work/ca.key" -out "$work/ca.crt" -days 2 -subj "/CN=pbh-qualify-ca" 2>/dev/null
openssl req -newkey rsa:2048 -nodes -keyout "$work/srv.key" -out "$work/srv.csr" -subj "/CN=pkg.example.test" 2>/dev/null
printf 'subjectAltName=DNS:pkg.example.test\n' > "$work/ext"
openssl x509 -req -in "$work/srv.csr" -CA "$work/ca.crt" -CAkey "$work/ca.key" -CAcreateserial -out "$work/srv.crt" -days 2 -extfile "$work/ext" 2>/dev/null
cat > "$work/serve.py" <<'PY'
import http.server, ssl
class Handler(http.server.SimpleHTTPRequestHandler):
    def __init__(self, *args, **kwargs):
        super().__init__(*args, directory="/srv/site", **kwargs)
    def translate_path(self, path):
        return super().translate_path("/" + path.split("?")[0].rsplit("/", 1)[-1])
server = http.server.ThreadingHTTPServer(("127.0.0.1", 9443), Handler)
context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
context.load_cert_chain("/srv/rig/srv.crt", "/srv/rig/srv.key")
server.socket = context.wrap_socket(server.socket, server_side=True)
server.serve_forever()
PY
 fresh_host_up --add-host pkg.example.test:127.0.0.1
 inside 'mkdir -p /srv/site /srv/rig'
 $docker cp "$work/site/." "$name:/srv/site/"
 for f in srv.crt srv.key serve.py; do $docker cp "$work/$f" "$name:/srv/rig/$f"; done
 $docker cp "$work/ca.crt" "$name:/usr/local/share/ca-certificates/pbh-qualify-ca.crt"
 inside 'update-ca-certificates >/dev/null 2>&1; (nohup python3 -I /srv/rig/serve.py >/var/log/origin.log 2>&1 &); sleep 1'
 installer='curl -fsSL https://pkg.example.test:9443/install | sh -s --'
else
 step "fresh $base host"
 fresh_host_up
 installer="curl -fsSL '$published' | sh -s --"
fi

if [ "$refusal" = true ]; then
 step "unsupported system is refused before anything changes"
 if inside "$installer --enable relay --endpoint-host 203.0.113.10" > "$work/out" 2>&1; then fail "installer accepted $base"; fi
 grep -qi 'unsupported' "$work/out" || { cat "$work/out"; fail "refusal does not say the system is unsupported"; }
 inside '! command -v docker && ! test -e /usr/local/bin/pbh && ! test -e /etc/systemd/system/paperboat-selfhost.service' || fail "refused install changed the host"
 echo "PASS refusal on $base"; exit 0
fi

host=203.0.113.10
endpoint="--endpoint-host $host"
[ -z "$published" ] || endpoint=''
step "fresh install --enable $enable"
inside "$installer --enable $enable $endpoint" > "$work/out" 2>&1 || { tail -30 "$work/out"; fail "install failed"; }
grep -qE "^Claim address: [0-9a-f.:]+:8443" "$work/out" || fail "claim address not printed"
[ -n "$published" ] || grep -q "^Claim address: $host:8443" "$work/out" || fail "claim address is not the endpoint host"
grep -qE '^Self-host code: pbh1\.' "$work/out" || fail "claim code not printed"
inside 'systemctl is-active --quiet paperboat-selfhost.service' || fail "service not active"
inside 'systemctl is-enabled --quiet paperboat-selfhost.service' || fail "service not enabled"
for capability in $(echo "$enable" | sed 's/both/relay tunnel/'); do
 grep -q "^Preparing $capability image " "$work/out" || fail "$capability image was not prepared by the installer"
 inside "docker image inspect \"\$(sed -E 's/.*\"$capability\":\"([^\"]*)\".*/\\1/' /var/lib/paperboat-selfhost/images.json)\" >/dev/null" || fail "$capability image not present"
done
inside 'ss -ltn | grep -q ":8443 "' || fail "claim API not listening"
inside 'test "$(stat -c %a /var/lib/paperboat-selfhost)" = 700' || fail "state directory is not owner-only"
before=$(inside 'sha256sum /var/lib/paperboat-selfhost/tls.crt')

step "re-run is idempotent and keeps identity"
inside "$installer --enable $enable $endpoint" > "$work/out2" 2>&1 || { tail -30 "$work/out2"; fail "re-run failed"; }
inside 'systemctl is-active --quiet paperboat-selfhost.service' || fail "service not active after re-run"
[ "$before" = "$(inside 'sha256sum /var/lib/paperboat-selfhost/tls.crt')" ] || fail "re-run changed the installation identity"

step "uninstall keeps data, reinstall reuses it, confirmed removal deletes it"
inside 'pbh uninstall' >/dev/null 2>&1 || fail "uninstall failed"
inside '! test -e /etc/systemd/system/paperboat-selfhost.service && ! test -e /usr/local/bin/pbh && test -z "$(docker ps -aq --filter label=io.paperboat.pbh-owner)"' || fail "uninstall left service, command or containers"
inside 'test -f /var/lib/paperboat-selfhost/tls.crt' || fail "uninstall removed data without confirmation"
inside "$installer --enable $enable $endpoint" > "$work/out3" 2>&1 || fail "reinstall failed"
[ "$before" = "$(inside 'sha256sum /var/lib/paperboat-selfhost/tls.crt')" ] || fail "reinstall did not reuse the retained identity"
inside 'pbh uninstall --remove-data --confirm-remove-data' >/dev/null 2>&1 || fail "confirmed removal failed"
inside '! test -e /var/lib/paperboat-selfhost/tls.crt' || fail "confirmed removal kept data"
echo "PASS $enable on $base ($arch)"
