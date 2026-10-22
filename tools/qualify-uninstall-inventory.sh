#!/bin/sh
# C021 (Linux): `pb uninstall` on an enrolled machine removes every Paperboat service, unit, binary,
# state, configuration, credential and process, and keeps only the Inbox.
#   tools/qualify-uninstall-inventory.sh --run "ssh pbvm2 sudo docker exec -i CT" --user pbt \
#     --as-user "ssh pbvm2 sudo docker exec -i -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 CT"
# --run runs a command as root on the machine (a throwaway container); --as-user runs it as the enrolled user.
set -eu
run='' user='' as_user=''
while [ "$#" -gt 0 ]; do
 case "$1" in --run) run=$2; shift 2;; --user) user=$2; shift 2;; --as-user) as_user=$2; shift 2;; *) echo "unknown argument: $1" >&2; exit 2;; esac
done
[ -n "$run" ] && [ -n "$user" ] && [ -n "$as_user" ] || { echo '--run, --user and --as-user are required' >&2; exit 2; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
out=$(printf '%s\n' 'H=$(hostname); ~/.local/bin/pb uninstall --confirmation "UNINSTALL PAPERBOAT" --hostname "$H" --json' | $as_user sh -s 2>&1) || fail "pb uninstall failed: $(echo "$out" | tail -c 300)"
echo "$out" | grep -q '"uninstalled":true' || fail "uninstall did not report uninstalled"
echo "$out" | grep -q '"inbox_preserved":true' || fail "uninstall did not report the Inbox preserved"
left=$({ echo "user=$user"; cat <<'SCRIPT'; } | $run sh -s
H=$(getent passwd $user | cut -d: -f6)
systemctl list-unit-files --no-legend 2>/dev/null | grep -i paperboat
ls -d /etc/systemd/system/*paperboat* /usr/local/libexec/paperboat /var/lib/paperboat /run/paperboat* $H/.local/state/paperboat $H/.config/paperboat $H/.local/bin/pb $H/.config/systemd/user/*paperboat* /etc/sudoers.d/*paperboat* /usr/local/share/ca-certificates/*aperboat* 2>/dev/null
ps -eo args | grep -v grep | grep -E 'paperboat|/pb (daemon|__)'
getent passwd | grep -i paperboat
true
SCRIPT
)
[ -z "$left" ] || fail "uninstall left: $(echo "$left" | tr '\n' ' ' | cut -c1-400)"
echo "test -d \$(getent passwd $user | cut -d: -f6)/Paperboat/Inbox" | $run sh -s || fail "the Inbox was not preserved"
echo "PASS uninstall removed every Paperboat service, file, credential and process; Inbox kept"
