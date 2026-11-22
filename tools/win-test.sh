#!/usr/bin/env bash
# Run a module's Go tests on Windows the way a normal user's session does. Run it from the module directory
# (cli or shared) in an INTERACTIVE session: a GitHub Actions step, or tools/win-remote-test.sh on a winpool machine.
# A raw SSH logon is a network session (no console, no pipes or child processes for some tests, git cannot create
# folders): its results are not signal.
#   env: PACKAGES (default ./...)  RUN (go test -run)  COUNT (default 1)  RACE (-race or empty)  EVENTS (dir for go test -json logs)
# The runner is an elevated administrator, so what a test creates is owned by Administrators, which the credential and
# lock checks refuse. win-test-exec runs each test binary with an unelevated token and a private TEMP/APPDATA root that
# token owns. The tests in elevated_tests.txt need Administrators/SYSTEM ownership and run second, elevated.
# Every failure is collected (nothing stops at the first); the exit status is 1 if any package or check failed.
set -uo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
PACKAGES=${PACKAGES:-./...} COUNT=${COUNT:-1} RACE=${RACE:-} RUN=${RUN:-}
EVENTS=${EVENTS:-${RUNNER_TEMP:-${TEMP:-/tmp}}/gotest-events}
mkdir -p "$EVENTS"
rm -f "$EVENTS"/*.jsonl
[ -z "$RACE" ] || export CGO_ENABLED=1 # -race needs cgo and gcc (the hosted images ship mingw)

# A package list given by hand (a shard of tools/ci-shards.py) may name packages this OS excludes by build constraints,
# which go test refuses as "setup failed"; keep the packages that have tests here.
if [ "$PACKAGES" != "./..." ]; then
  PACKAGES=$(go list -e -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' $PACKAGES)
  [ -n "$PACKAGES" ] || { echo "no test packages for Windows in this shard"; exit 0; }
fi

exec_dir=$(cygpath -m "$(mktemp -d)") # no spaces: go test -exec splits its value on spaces
trap 'rm -rf "$exec_dir"' EXIT
(cd "$root/cli" && go build -o "$exec_dir/win-test-exec.exe" ./tools/win-test-exec) || { echo "win-test-exec did not build"; exit 1; }

listed=$(grep -v '^#' "$root/cli/tools/win-test-exec/elevated_tests.txt" | grep .)
all_elevated=$(paste -sd'|' <<<"$listed")
# A narrowed run runs only the elevated tests it names (the top-level part of -run, before the first /).
elevated=$listed
[ -z "$RUN" ] || elevated=$(grep -E -- "${RUN%%/*}" <<<"$listed" || true)
[ "$(basename "$PWD")" = cli ] || elevated= # the list names cli tests

report() { python3 "$root/tools/gotest-json.py" --save "$EVENTS/$1.jsonl" ${RUN:+--verbose}; }
status=0
go test -json $RACE -exec "$exec_dir/win-test-exec.exe" -skip "^($all_elevated)\$" -timeout 15m -count "$COUNT" \
  ${RUN:+-run "$RUN"} $PACKAGES | report main || status=1
if [ -n "$elevated" ]; then
  go test -json $RACE -exec "$exec_dir/win-test-exec.exe -elevated" -run "^($(paste -sd'|' <<<"$elevated"))\$" \
    -timeout 15m -count "$COUNT" $PACKAGES | report elevated || status=1
  # A renamed or deleted elevated test would make the second pass run nothing and look green.
  if [ -z "$RUN" ] && [ "$PACKAGES" = "./..." ]; then
    while read -r name; do
      grep -E '"Action":"(pass|fail)"' "$EVENTS/elevated.jsonl" | grep -qF "\"Test\":\"$name\"," ||
        { echo "elevated test $name never ran: fix or remove it in cli/tools/win-test-exec/elevated_tests.txt"; status=1; }
    done <<<"$elevated"
  fi
fi

# What ran: counts and every skipped test (a skip is a test that did not run on Windows).
summary=$(python3 - "$EVENTS"/*.jsonl <<'PY'
import collections, json, sys
counts, skipped, broken = collections.Counter(), [], []
for path in sys.argv[1:]:
    for line in open(path):
        try:
            event = json.loads(line)
        except ValueError:
            continue
        if not event.get('Test') and event.get('Action') == 'fail':
            broken.append(event['Package'].rsplit('/cli/', 1)[-1])
        if event.get('Test') and event.get('Action') in ('pass', 'fail', 'skip'):
            counts[event['Action']] += 1
            if event['Action'] == 'skip':
                skipped.append('%s %s' % (event['Package'].rsplit('/internal/', 1)[-1], event['Test']))
print('tests: %d passed, %d failed, %d skipped; failed packages: %s' % (counts['pass'], counts['fail'], counts['skip'], ' '.join(sorted(set(broken))) or 'none'))
print('\n'.join('skipped: ' + s for s in sorted(skipped)))
PY
)
echo "$summary"
[ -z "${GITHUB_STEP_SUMMARY:-}" ] || echo "$summary" >> "$GITHUB_STEP_SUMMARY"
exit $status
