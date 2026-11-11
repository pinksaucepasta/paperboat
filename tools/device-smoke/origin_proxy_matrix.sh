#!/bin/bash
# Origin reverse-proxy matrix: Caddy, Traefik, nginx and Apache set up to force HTTPS (HTTP -> HTTPS
# redirect plus host-based virtual hosting), each reached the three ways a tunnel route can reach
# them. It runs the real origin transport (TestOriginProxyMatrix) and prints one table. Needs Docker
# on a Linux host that you own and a prebuilt test binary:
#   (cd cli && CGO_ENABLED=0 go test -c -o /tmp/origin.test ./internal/hostruntime/tunnelmanager)
#   DOCKER="sudo docker" PB_MATRIX_TEST_BIN=/tmp/origin.test tools/device-smoke/origin_proxy_matrix.sh
# PB_MATRIX_LOGS="httpd nginx" prints those containers' logs. Containers use the prefix tlshttps- and are removed on exit. Exit 1 when a promised cell fails.
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

for ct in ${PB_MATRIX_LOGS:-}; do echo "--- logs $ct"; $DOCKER logs --tail 15 "tlshttps-$ct" 2>&1; done
python3 - "$W/cells.jsonl" <<'PY'
import json, sys
cells = [json.loads(l) for l in open(sys.argv[1])]
modes = ["a-http", "b-https", "c-https-sni", "C8-sse", "C12-range", "C13-expect", "F-force-ssl"]
heads = ["a http origin", "b https origin", "c https+SNI", "SSE", "Range", "Expect", "force-ssl app"]
def text(c):
    r = c["result"]
    if r == "ok": return "ok"
    if r == "loop": return "LOOP"
    if r == "app_refuses": return "refuses"
    if r == "origin_error":
        d = str(c.get("detail", ""))
        return "tls-name" if "valid for" in d else "tls-alert" if "remote error" in d else "error"
    return r
print("%-15s" % "server" + "".join("%-16s" % h for h in heads))
for s in dict.fromkeys(c["server"] for c in cells):
    row = {c["mode"]: c for c in cells if c["server"] == s}
    print("%-15s" % s + "".join("%-16s" % (text(row[m]) if m in row else "-") for m in modes))
print("\nwhat the app saw through mode c (X-Forwarded-* the edge sent are: proto=https host=public for=client):")
for c in cells:
    if c["mode"] == "c-https-sni" and c["result"] == "ok":
        print("  %-15s host=%s xfp=%s xfh=%s xff=%s" % (c["server"], c.get("host"), c.get("xfp"), c.get("xfh"), c.get("xff")))
print()
for c in cells:
    if c["result"] != "ok": print("  %s / %s: %s %s" % (c["server"], c["mode"], c["result"], str(c.get("detail", ""))[:140]))
PY
