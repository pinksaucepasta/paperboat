#!/bin/sh
# Run ON a disposable Linux host (sh -s -- <published selfhost URL> over ssh; selfhost_rows.sh does): CHECKLIST S4-07, the
# pbh maintenance commands on a freshly installed, not yet claimed node. check-host, version, status, restart and
# selfhost code work; doctor passes its host checks and says the control-plane ones need a claim; logs, restart relay and
# update refuse and say to claim the node first; wrong arguments are refused. Removes the node and its data on exit.
set -u
url=$1; bad=0
trap 'sudo -n pbh uninstall --remove-data --confirm-remove-data >/dev/null 2>&1 || true' EXIT
curl -fsSL "$url" | sudo -n sh -s -- --both >/dev/null 2>&1 || { echo 'FAIL pbh install'; exit 1; }
cell() { # name expected-exit(zero|nonzero) pattern command...
  name=$1 want=$2 pattern=$3; shift 3
  out=$(timeout 200 "$@" 2>&1); rc=$?
  clean=$(printf '%s\n' "$out" | sed -E 's/pbh1\.[A-Za-z0-9._-]+/pbh1.<redacted>/g')
  if { [ "$want" = zero ] && [ $rc != 0 ]; } || { [ "$want" = nonzero ] && [ $rc = 0 ]; } || ! printf '%s\n' "$out" | grep -qE "$pattern"; then
    echo "FAIL $name: exit $rc (wanted $want, /$pattern/)"; printf '%s\n' "$clean" | tail -6; bad=1
  else echo "ok   $name"; fi
}
cell check-host zero 'Supported self-host operating system' sudo -n pbh check-host
cell version zero '^pbh 20[0-9]{2}\.' pbh version
cell help zero 'check-host' pbh help
cell status zero 'Active: active' sudo -n pbh status
cell doctor-unclaimed nonzero 'finish claiming this installation' sudo -n pbh doctor
cell doctor-host-checks nonzero 'ok +host firewall' sudo -n pbh doctor
cell logs-unclaimed nonzero 'claim this node in Dashboard' sudo -n pbh logs --lines 5
cell restart zero 'Restarted paperboat-selfhost' sudo -n pbh restart
cell active-after-restart zero '^active$' systemctl is-active paperboat-selfhost.service
cell restart-relay-unclaimed nonzero 'claim this node in Dashboard' sudo -n pbh restart relay
cell update-unclaimed nonzero 'not claimed yet' sudo -n pbh update
cell selfhost-code zero '^Self-host code: pbh1\.' sudo -n pbh selfhost code
cell logs-wrong-argument nonzero 'use pbh logs' sudo -n pbh logs nosuch
[ $bad = 0 ] && echo 'PASS pbh check-host, version, status, doctor, logs, restart, update and selfhost code on an unclaimed node'
exit $bad
