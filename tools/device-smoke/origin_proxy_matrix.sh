#!/bin/bash
# Origin reverse-proxy matrix: Caddy, Traefik, nginx and Apache set up to force HTTPS (HTTP -> HTTPS
# redirect plus host-based virtual hosting), each reached the three ways a tunnel route can reach
# them. It runs the real origin transport (TestOriginProxyMatrix) and prints one table. Needs Docker
# on a Linux host that you own and a prebuilt test binary:
#   (cd cli && CGO_ENABLED=0 go test -c -o /tmp/origin.test ./internal/hostruntime/tunnelmanager)
#   DOCKER="sudo docker" PB_MATRIX_TEST_BIN=/tmp/origin.test tools/device-smoke/origin_proxy_matrix.sh
# Containers use the prefix tlshttps- and are removed on exit. Exit 1 when a promised cell fails.
set -euo pipefail
DOCKER=${DOCKER:-docker}
BIN=${PB_MATRIX_TEST_BIN:?set PB_MATRIX_TEST_BIN to a go test -c binary of internal/hostruntime/tunnelmanager}
HERE=$(cd "$(dirname "$0")/origin_proxy_matrix" && pwd)
HOST=app.example.test
W=$(mktemp -d)
NET=tlshttps-net
cleanup() { $DOCKER rm -f $($DOCKER ps -aq --filter name=tlshttps-) >/dev/null 2>&1 || true; $DOCKER network rm $NET >/dev/null 2>&1 || true; rm -rf "$W"; }
trap cleanup EXIT
cleanup; mkdir -p "$W/certs"; $DOCKER network create $NET >/dev/null

# one CA that stands in for the host's system trust store, and one leaf for the public hostname
openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj /CN=matrix-ca -keyout "$W/ca.key" -out "$W/ca.pem" 2>/dev/null
openssl req -newkey rsa:2048 -nodes -subj "/CN=$HOST" -keyout "$W/certs/leaf.key" -out "$W/leaf.csr" 2>/dev/null
printf "subjectAltName=DNS:%s\n" "$HOST" > "$W/san.ext"
openssl x509 -req -in "$W/leaf.csr" -CA "$W/ca.pem" -CAkey "$W/ca.key" -CAcreateserial -days 2 -extfile "$W/san.ext" -out "$W/certs/leaf.crt" 2>/dev/null
chmod 644 "$W/certs/leaf.key"
sed "s/@HOST@/$HOST/g" "$HERE/traefik-dynamic.yml.tmpl" > "$W/traefik-dynamic.yml"
sed "s/@HOST@/$HOST/g" "$HERE/nginx.conf.tmpl" > "$W/nginx.conf"
sed "s/@HOST@/$HOST/g" "$HERE/httpd.conf.tmpl" > "$W/httpd.conf"

$DOCKER run -d --name tlshttps-app --network $NET --network-alias app -v "$HERE/app.py:/app.py:ro" python:3-alpine python /app.py >/dev/null
caddy() { # name, Caddyfile, host ports
  $DOCKER run -d --name "tlshttps-$1" --network $NET -e PUBLIC_HOST=$HOST -p "127.0.0.1:$3:80" -p "127.0.0.1:$4:443" \
    -v "$HERE/$2:/etc/caddy/Caddyfile:ro" -v "$W/certs:/certs:ro" caddy:2 >/dev/null
}
caddy caddy Caddyfile.certs 11080 11443
caddy caddy-internal Caddyfile.internal 12080 12443
caddy caddy-acme Caddyfile.acme 13080 13443
$DOCKER run -d --name tlshttps-traefik --network $NET -p 127.0.0.1:14080:80 -p 127.0.0.1:14443:443 \
  -v "$HERE/traefik-static.yml:/etc/traefik/traefik.yml:ro" -v "$W/traefik-dynamic.yml:/etc/traefik/dynamic.yml:ro" -v "$W/certs:/certs:ro" traefik:v3 >/dev/null
$DOCKER run -d --name tlshttps-nginx --network $NET -p 127.0.0.1:15080:80 -p 127.0.0.1:15443:443 \
  -v "$W/nginx.conf:/etc/nginx/nginx.conf:ro" -v "$W/certs:/certs:ro" nginx:alpine >/dev/null
$DOCKER run -d --name tlshttps-httpd --network $NET -p 127.0.0.1:16080:80 -p 127.0.0.1:16443:443 \
  -v "$W/httpd.conf:/usr/local/apache2/conf/httpd.conf:ro" -v "$W/certs:/certs:ro" httpd:2.4-alpine >/dev/null

sleep 8 # Caddy writes its internal CA while it loads the config
$DOCKER exec tlshttps-caddy-internal cat /data/caddy/pki/authorities/local/root.crt > "$W/caddy-root.pem"
cat "$W/ca.pem" "$W/caddy-root.pem" > "$W/trust.pem"

PB_PROXY_HOST=$HOST SSL_CERT_FILE="$W/trust.pem" SSL_CERT_DIR=/nonexistent \
PB_PROXY_MATRIX="caddy=11080,11443;caddy-internal=12080,12443;caddy-acme=13080,13443;traefik=14080,14443;nginx=15080,15443;httpd=16080,16443" \
  "$BIN" -test.run TestOriginProxyMatrix -test.v | grep '^MATRIX ' | sed 's/^MATRIX //' > "$W/cells.jsonl"

python3 - "$W/cells.jsonl" <<'PY'
import json, sys
cells = [json.loads(l) for l in open(sys.argv[1])]
modes = ["a-http", "b-https", "c-https-sni"]
print("%-15s %-30s %-30s %-30s" % ("server", *["a: http origin", "b: https origin", "c: https + public host SNI"]))
failed = 0
def text(c):
    r = c["result"]
    if r == "ok": return "ok host=%s xfp=%s" % (c.get("host"), c.get("xfp"))
    if r == "loop": return "LOOP (%d hops)" % c["hops"]
    if r == "app_refuses": return "app refuses (xfp=%s)" % c.get("xfp")
    return "%s: %s" % (r, str(c.get("detail", ""))[:60])
for s in dict.fromkeys(c["server"] for c in cells):
    row = {c["mode"]: c for c in cells if c["server"] == s}
    print("%-15s %-30s %-30s %-30s" % (s, *[text(row[m])[:29] for m in modes]))
print()
for c in cells:
    if c["result"] != "ok": print("  %s / %s: %s %s" % (c["server"], c["mode"], c["result"], c.get("detail", "")))
PY
