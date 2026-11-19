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
  --workspace) [ -z "\${FAKE_TEAM_FAIL:-}" ] || { echo 'team machine unreachable' >&2; exit 1; }; shift 2; exec "\$0" "\$@" ;;
  exec) [ -z "\${FAKE_FAIL:-}" ] || { echo 'machine unreachable' >&2; exit 1; }; shift 3; "\$@" ;;
  ssh) shift 3; "\$@" ;;
  machine) echo '{"data":{"items":[{"name":"b"},{"name":"a"}]}}' ;;
  send) cp "\$2" "\$HOME/inbox/"; echo "\$(basename \$2): delivered to \$HOME/inbox on peer" ;;
  inbox) echo '{"data":{"path":"'"\$HOME"'/inbox"}}' ;;
  preview) [ "\$2" = stop ] || echo '{"data":{"id":"p1","endpoint":"http://'"\${FAKE_PREVIEW_HOST:-127.0.0.1}"':'"\$2"'"}}' ;;
esac
FAKE
chmod +x "$d/bin/pb"
for _ in $(seq 50); do curl -fs "http://127.0.0.1:$port/" >/dev/null 2>&1 && break; sleep 0.1; done
export RUN_A="env PATH=$d/bin:$PATH HOME=$d/home CANARY_T=fixture" RUN_B="env PATH=$d/bin:$PATH HOME=$d/home CANARY_T=fixture" PB=pb
export CANARY_A=a CANARY_B=b CANARY_PORT=$port CANARY_TUNNEL_URL=http://127.0.0.1:$port PROBE_STEP_TIMEOUT=10 CANARY_ENV_NAME=CANARY_T CANARY_ENV_VALUE=fixture
out=$("$here/probe.sh") || { echo "healthy run failed: $out"; exit 1; }
[ "$(grep -c '"ok":true' <<<"$out")" = 9 ] || { echo "healthy run: $out"; exit 1; }
[ -z "$(ls "$d/home/inbox")" ] || { echo "received file left in the Inbox"; exit 1; }
# A value that did not arrive fails the env_exec step only (and never prints the expected value).
if out=$(CANARY_ENV_VALUE=other "$here/probe.sh"); then echo "wrong ENV value passed: $out"; exit 1; fi
grep -q '"step":"env_exec","ok":false' <<<"$out" && [ "$(grep -c '"ok":false' <<<"$out")" = 1 ] && ! grep -q other <<<"$out" || { echo "env_exec: $out"; exit 1; }
if out=$(FAKE_FAIL=1 "$here/probe.sh"); then echo "broken exec passed: $out"; exit 1; fi
grep -q '"step":"exec_a_to_b","ok":false,"seconds":[0-9]*,"error":"machine unreachable"' <<<"$out"
grep -q '"step":"exec_b_to_a","ok":false' <<<"$out"
[ "$(grep -c '"ok":true' <<<"$out")" = 6 ] || { echo "broken exec run: $out"; exit 1; }
# A team machine is probed in the team workspace when configured; its failure fails team_exec only.
out=$(CANARY_TEAM=t CANARY_TEAM_MACHINE=c "$here/probe.sh") || { echo "team run failed: $out"; exit 1; }
grep -q '"step":"team_exec","ok":true' <<<"$out" || { echo "team step missing: $out"; exit 1; }
if out=$(FAKE_TEAM_FAIL=1 CANARY_TEAM=t CANARY_TEAM_MACHINE=c "$here/probe.sh"); then echo "broken team exec passed: $out"; exit 1; fi
grep -q '"step":"team_exec","ok":false,"seconds":[0-9]*,"error":"team machine unreachable"' <<<"$out" && [ "$(grep -c '"ok":false' <<<"$out")" = 1 ] || { echo "team_exec: $out"; exit 1; }
# Every configured edge is fetched directly; one that does not answer fails the edges step only.
out=$(CANARY_EDGES="127.0.0.1" "$here/probe.sh") || { echo "edges run failed: $out"; exit 1; }
grep -q '"step":"edges","ok":true' <<<"$out" || { echo "edges step missing: $out"; exit 1; }
# Each edge address must serve this run's own preview, one step per address: here edge.test reaches the origin through 127.0.0.1
# but 127.0.0.2 refuses, so only that edge fails; the preview step itself fails too because edge.test is not in DNS.
out=$(PROBE_STEPS="preview edge_127.0.0.1 edge_127.0.0.2" FAKE_PREVIEW_HOST=edge.test CANARY_EDGES="127.0.0.1 127.0.0.2" "$here/probe.sh") && { echo "dead edge passed: $out"; exit 1; }
grep -q '"step":"edge_127.0.0.1","ok":true' <<<"$out" && grep -q '"step":"edge_127.0.0.2","ok":false,"seconds":[0-9]*,"error":"edge 127.0.0.2:' <<<"$out" || { echo "per-edge preview: $out"; exit 1; }
# Without a preview there is no verdict on an edge (a lost canary machine must not read as every edge down).
out=$(PROBE_STEPS="preview edge_127.0.0.1" CANARY_EDGES="127.0.0.1" PB=false "$here/probe.sh") && { echo "preview without pb passed: $out"; exit 1; }
! grep -q '"step":"edge_' <<<"$out" || { echo "edge verdict without a preview: $out"; exit 1; }
# A preview made from B works the same way (anvit's vantage point has only B).
out=$(PROBE_STEPS="preview edge_127.0.0.1" PREVIEW_ON=B CANARY_EDGES="127.0.0.1" "$here/probe.sh") || { echo "B preview run failed: $out"; exit 1; }
[ "$(grep -c '"ok":true' <<<"$out")" = 2 ] || { echo "B preview: $out"; exit 1; }
# A step list runs only those steps, and the tunnel URL is not needed when the tunnel step is not wanted.
out=$(PROBE_STEPS="exec_a_to_b exec_b_to_a" CANARY_TUNNEL_URL= "$here/probe.sh") || { echo "step list run failed: $out"; exit 1; }
[ "$(grep -c '"ok":true' <<<"$out")" = 2 ] && [ "$(grep -c '"step"' <<<"$out")" = 2 ] || { echo "step list: $out"; exit 1; }
# Steps driven from B run only when named, and pass with the same fake machines.
out=$(PROBE_STEPS="exec_b_to_a ssh_b_to_a send_b_to_a api_auth_b" CANARY_TUNNEL_URL= "$here/probe.sh") || { echo "B-driven run failed: $out"; exit 1; }
[ "$(grep -c '"ok":true' <<<"$out")" = 4 ] || { echo "B-driven steps: $out"; exit 1; }
echo "probe self-check ok"
