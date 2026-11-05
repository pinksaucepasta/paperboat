#!/bin/bash
# F18 device check (PLAN F18-10, CHECKLIST C244-C253): pb policy get/set/test/explain/versions/
# restore/schema and pb api-token create/list/revoke against the live server, in the personal
# workspace of an enrolled throwaway container on a docker host (pbvm2/pbvm3 harness).
# Usage: CT=<container> EMAIL=<account email> TARGET=<machine name in the account> policy_cli.sh
#    or, on a paired machine itself (macOS): LOCAL=1 EMAIL=... TARGET=... policy_cli.sh
# Prints PASS/FAIL per step and exits non-zero on any FAIL; never prints secrets. The personal
# policy is restored to the version it had before the run.
set -u
EMAIL=${EMAIL:?account email} TARGET=${TARGET:?machine name}
if [ -n "${LOCAL:-}" ]; then
  P="$HOME/.local/bin/pb" D=$HOME
  put() { cp "$1" "$D/$(basename "$1")"; }
else
  CT=${CT:?container} D=/home/pbt
  P="sudo docker exec -i -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 $CT timeout 90 /home/pbt/.local/bin/pb"
  put() { sudo docker cp "$1" "$CT:$D/$(basename "$1")" >/dev/null; sudo docker exec "$CT" chown pbt:pbt "$D/$(basename "$1")"; }
fi
fails=0
ok(){ echo "PASS $1"; } ; bad(){ echo "FAIL $1: $2"; fails=$((fails+1)); }
out=$($P policy schema 2>&1); echo "$out" | python3 -c "import sys,json;d=json.load(sys.stdin);assert d['title']=='Paperboat workspace policy'" 2>/dev/null && ok C250-schema || bad C250-schema "$(echo "$out"|head -2)"
out=$($P policy get --workspace personal --json 2>&1); v=$(echo "$out" | python3 -c "import sys,json;d=json.load(sys.stdin);d=d.get('data',d);print(d['version'],d['can_edit'])" 2>/dev/null) && ok "C244-get v=$v" || bad C244-get "$(echo "$out"|head -3)"
ver=${v%% *}
out=$($P policy versions --workspace personal --json 2>&1); echo "$out" | python3 -c "import sys,json;d=json.load(sys.stdin);d=d.get('data',d);assert len(d['versions'])>=1" 2>/dev/null && ok C248-versions || bad C248-versions "$(echo "$out"|head -3)"
cat > /tmp/lkpol.hujson <<J
{
  // F18 device check: tag the throwaway machine $TARGET
  "tags": {"tag:smoke-ci": ["machine:$TARGET"]},
  "rules": [],
  "tests": [{"src": "user:$EMAIL", "dst": "machine:$TARGET", "allow": ["exec", "terminal"]}],
}
J
put /tmp/lkpol.hujson
out=$($P policy test --workspace personal $D/lkpol.hujson 2>&1); rc=$?; [ $rc -eq 0 ] && ok C246-test || bad C246-test "rc=$rc $(echo "$out"|tail -3)"
printf '{"rules": [{"src": ["*"], "dst": ["machine:nope"], "allow": ["exec"]}]}' > /tmp/lkbad.json; put /tmp/lkbad.json
out=$($P policy test --workspace personal $D/lkbad.json 2>&1); rc=$?; [ $rc -ne 0 ] && echo "$out" | grep -q "/rules/0/dst/0: unknown_machine" && ok C246-test-invalid || bad C246-test-invalid "rc=$rc $(echo "$out"|tail -3)"
out=$($P policy set --workspace personal $D/lkpol.hujson --message "F18 device check" --yes 2>&1); rc=$?; [ $rc -eq 0 ] && ok C245-set || bad C245-set "rc=$rc $(echo "$out"|tail -3)"
out=$($P policy get --workspace personal 2>&1); echo "$out" | grep -q "tag:smoke-ci" && echo "$out" | grep -q "// F18 device check" && ok C244-get-file-with-comments || bad C244-get-file "$(echo "$out"|head -3)"
out=$($P policy explain --workspace personal --src user:$EMAIL --dst machine:$TARGET --action exec --json 2>&1); echo "$out" | python3 -c "import sys,json;d=json.load(sys.stdin);d=d.get('data',d);x=d['decision'];assert x['allowed'] and x['reason']=='owner'" 2>/dev/null && ok C247-explain-owner || bad C247-explain "$(echo "$out"|head -3)"
out=$($P policy restore --workspace personal $ver --yes 2>&1); rc=$?; [ $rc -eq 0 ] && ok "C249-restore v$ver" || bad C249-restore "rc=$rc $(echo "$out"|tail -3)"
out=$($P policy versions --workspace personal --json 2>&1); echo "$out" | python3 -c "import sys,json;d=json.load(sys.stdin);d=d.get('data',d);k=[v['kind'] for v in d['versions']][:2];assert k==['restored','saved'],k" 2>/dev/null && ok C248-versions-kinds || bad C248-versions-kinds "$(echo "$out"|head -3)"
out=$($P api-token create smoke-ci --workspace personal --expires 1d --scope "[{\"dst\":[\"machine:$TARGET\"],\"allow\":[\"exec\"]}]" --json 2>&1); echo "$out" | python3 -c "import sys,json;d=json.load(sys.stdin);d=d.get('data',d);assert d['secret'].startswith('pbat_') and d['api_token']['name']=='smoke-ci'" 2>/dev/null && ok C251-create || bad C251-create "$(echo "$out" | sed 's/pbat_[A-Za-z0-9_-]*/pbat_REDACTED/g' | head -3)"
out=$($P api-token list --workspace personal 2>&1); echo "$out" | grep -q "smoke-ci" && ! echo "$out" | grep -q "pbat_" && ok C252-list || bad C252-list "$(echo "$out" | sed 's/pbat_[A-Za-z0-9_-]*/pbat_REDACTED/g'|head -3)"
out=$($P api-token revoke smoke-ci --workspace personal --yes 2>&1); rc=$?; [ $rc -eq 0 ] && echo "$out" | grep -q "Revoked API token smoke-ci" && ok C253-revoke || bad C253-revoke "rc=$rc $(echo "$out"|tail -2)"
rm -f /tmp/lkpol.hujson /tmp/lkbad.json; [ -z "${LOCAL:-}" ] || rm -f "$D/lkpol.hujson" "$D/lkbad.json"
exit $fails
