#!/usr/bin/env bash
# Self-check for probe.sh with a fake pb: a healthy run passes every step, a failing exec stops it.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd); d=$(mktemp -d); trap 'rm -rf "$d"' EXIT
cat >"$d/pb" <<'FAKE'
#!/usr/bin/env bash
case $1 in
  exec) [ -z "${FAKE_FAIL:-}" ] || { echo down >&2; exit 1; }
        shift 2; [ "$1" = echo ] && { echo "$2"; exit 0; }
        [ "$1" = "$0" ] || [ "$1" = "$PB_BIN" ] && { echo '{"url":"http://127.0.0.1:1/x"}'; exit 0; }
        exit 0 ;;
  send) exit 0 ;;
esac
FAKE
chmod +x "$d/pb"
export PB_BIN="$d/pb" PROBE_MACHINE_A=a PROBE_MACHINE_B=b PROBE_STEP_TIMEOUT=10
out=$(FAKE_FAIL=1 "$here/probe.sh" || true)
grep -q '"step":"exec_a","ok":false' <<<"$out" && ! grep -q exec_b <<<"$out"
out=$("$here/probe.sh" || true)
grep -q '"step":"exec_a","ok":true' <<<"$out" && grep -q '"step":"send_file","ok":true' <<<"$out"
echo "probe self-check ok"
