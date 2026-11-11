#!/usr/bin/env bash
# Self-check for probe.sh with a fake pb and a local origin: a healthy run passes every step; a broken exec
# fails both exec steps and nothing else, and the run exits 1.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd); d=$(mktemp -d)
mkdir -p "$d/bin" "$d/home/canary-www" "$d/home/inbox"
port=$((20000 + RANDOM % 20000))
python3 -m http.server "$port" --bind 127.0.0.1 --directory "$d/home/canary-www" >/dev/null 2>&1 &
server=$!; trap 'kill $server; rm -rf "$d"' EXIT
cat >"$d/bin/pb" <<FAKE
#!/usr/bin/env bash
case \$1 in
  exec) [ -z "\${FAKE_FAIL:-}" ] || { echo 'machine unreachable' >&2; exit 1; }; shift 3; "\$@" ;;
  ssh) shift 3; "\$@" ;;
  send) cp "\$2" "\$HOME/inbox/" ;;
  inbox) echo '{"data":{"path":"'"\$HOME"'/inbox"}}' ;;
  preview) [ "\$2" = stop ] || echo '{"data":{"id":"p1","endpoint":"http://127.0.0.1:$port"}}' ;;
esac
FAKE
chmod +x "$d/bin/pb"
for _ in $(seq 50); do curl -fs "http://127.0.0.1:$port/" >/dev/null 2>&1 && break; sleep 0.1; done
export RUN_A="env PATH=$d/bin:$PATH HOME=$d/home" RUN_B="env PATH=$d/bin:$PATH HOME=$d/home" PB=pb
export CANARY_A=a CANARY_B=b CANARY_PORT=$port CANARY_TUNNEL_URL=http://127.0.0.1:$port PROBE_STEP_TIMEOUT=10
out=$("$here/probe.sh") || { echo "healthy run failed: $out"; exit 1; }
[ "$(grep -c '"ok":true' <<<"$out")" = 7 ] || { echo "healthy run: $out"; exit 1; }
[ -z "$(ls "$d/home/inbox")" ] || { echo "received file left in the Inbox"; exit 1; }
if out=$(FAKE_FAIL=1 "$here/probe.sh"); then echo "broken exec passed: $out"; exit 1; fi
grep -q '"step":"exec_a_to_b","ok":false,"seconds":[0-9]*,"error":"machine unreachable"' <<<"$out"
grep -q '"step":"exec_b_to_a","ok":false' <<<"$out"
[ "$(grep -c '"ok":true' <<<"$out")" = 5 ] || { echo "broken exec run: $out"; exit 1; }
echo "probe self-check ok"
