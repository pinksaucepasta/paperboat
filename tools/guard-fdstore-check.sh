#!/bin/sh
# Does a daemon-reload after the machine guard's unit file is gone still leave systemd (PID 1) standing?
# systemd 255.4 (Ubuntu 24.04) aborts PID 1 when a unit with stored descriptors has no unit file (systemd #35288):
# the guard keeps one socket per lease in its descriptor store (FileDescriptorStorePreserve=yes), and three paths
# delete its unit file and reload. Each runs ITER times in a systemd container with the real guard holding LEASES
# stored sockets; the check fails when the container's init dies. One container per path, recreated after a crash.
#   rollback   a fresh `machine-guard install` made to fail after the guard started (systemctl shim): rollback
#   uninstall  `machine-guard uninstall`
#   purge      `__runtime-service purge` (the purge's guard step)
# usage: tools/guard-fdstore-check.sh <pb-linux-binary> <holder.test> [rollback|uninstall|purge|all] [ITER=25] [LEASES=17]
#   holder.test: (cd cli && go test -c -o holder.test ./internal/machineguard) for the same arch as the container.
# Needs docker (sudo -n) and the pb-canary image (tools/probe/canary.Dockerfile). Exit 0: init survived every round.
set -u
pb=$1 holder=$2 which=${3:-all} iter=${4:-25} leases=${5:-17}
here=$(cd "$(dirname "$0")" && pwd)
D="sudo -n docker"
$D image inspect pb-canary >/dev/null 2>&1 || $D build -q -t pb-canary - <"$here/probe/canary.Dockerfile" >/dev/null || exit 2
domain=harness.example.test

start() { # name
	$D rm -f "$1" >/dev/null 2>&1
	$D run -d --name "$1" --memory 768m --privileged --cgroupns=host -v /sys/fs/cgroup:/sys/fs/cgroup:rw --tmpfs /run --tmpfs /run/lock pb-canary >/dev/null || return 1
	for _ in $(seq 40); do $D exec "$1" systemctl is-system-running --wait >/dev/null 2>&1 && break; sleep 2; done
	$D cp "$pb" "$1:/root/pb" && $D cp "$holder" "$1:/root/holder.test" && $D exec "$1" chmod 755 /root/pb /root/holder.test
	$D exec -i "$1" sh -c 'cat > /root/round.sh' <<'ROUND'
# one round as root: $1 path, $2 browser domain, $3 leases
set -u
path=$1 domain=$2 leases=$3
pb=/root/pb
rm -f /run/fdstore-ready /run/fdstore-injected; mkdir -p /root/shim
cat > /root/shim/systemctl <<'SHIM'
#!/bin/sh
/usr/bin/systemctl "$@"; rc=$?
if [ "$rc" = 0 ] && [ "${1:-}" = restart ] && [ "${2:-}" = paperboat-machineguard.service ]; then
	for _ in $(seq 100); do [ -e /run/fdstore-ready ] && break; sleep 0.1; done
	systemctl show paperboat-machineguard.service -p NFileDescriptorStore --value >/run/fdstore-count
	touch /run/fdstore-injected; exit 1 # the install fails after the guard started and holds its leases
fi
exit $rc
SHIM
chmod 755 /root/shim/systemctl
hold() {
	PB_FDSTORE_HARNESS_LEASES=$leases PB_FDSTORE_HARNESS_READY=/run/fdstore-ready PB_FDSTORE_HARNESS_DOMAIN=$domain \
		/root/holder.test -test.run TestHoldLeasesForFDStoreHarness -test.timeout 5m >/root/holder.log 2>&1 &
	holder=$!
}
waitready() { for _ in $(seq 300); do [ -e /run/fdstore-ready ] && return 0; sleep 0.1; done; echo "guard never held its leases"; tail -n 5 /root/holder.log; return 1; }
case $path in
rollback)
	hold
	PATH=/root/shim:$PATH $pb daemon machine-guard install --browser-domain "$domain" >/root/op.log 2>&1 && { echo "injected install failure did not fail"; exit 3; }
	[ -e /run/fdstore-injected ] || { echo "install failed before the guard started:"; tail -n 5 /root/op.log; exit 3; }
	[ -e /run/fdstore-ready ] || { echo "guard never held its leases"; tail -n 5 /root/holder.log; exit 3; }
	[ "$(cat /run/fdstore-count)" -ge "$leases" ] || { echo "descriptor store held $(cat /run/fdstore-count), want $leases"; exit 3; }
	;;
uninstall|purge)
	$pb daemon machine-guard install --browser-domain "$domain" >/root/op.log 2>&1 || { echo "install:"; tail -n 5 /root/op.log; exit 3; }
	hold; waitready || exit 3
	stored=$(systemctl show paperboat-machineguard.service -p NFileDescriptorStore --value)
	[ "$stored" -ge "$leases" ] || { echo "descriptor store held $stored, want $leases"; exit 3; }
	if [ "$path" = uninstall ]; then $pb daemon machine-guard uninstall >/root/op.log 2>&1
	else PAPERBOAT_INVOKING_UID=0 $pb __runtime-service purge >/root/op.log 2>&1; fi
	;;
esac
kill $holder 2>/dev/null; wait $holder 2>/dev/null
exit 0
ROUND
}

alive() { # the container runs and its systemd answers
	[ "$($D inspect -f '{{.State.Running}}' "$1")" = true ] || return 1
	state=$(timeout 30 $D exec "$1" systemctl is-system-running 2>/dev/null); [ "$state" = running ] || [ "$state" = degraded ]
}

fail=0
for path in $([ "$which" = all ] && echo rollback uninstall purge || echo "$which"); do
	name=pb-fdstore-$path-$$
	start $name || { echo "$path: container did not start"; exit 2; }
	crashes=0 broken=0
	for i in $(seq "$iter"); do
		out=$($D exec $name sh /root/round.sh "$path" "$domain" "$leases" 2>&1); rc=$?
		if ! alive $name; then
			crashes=$((crashes + 1)); echo "$path round $i: init died (container exit $($D inspect -f '{{.State.ExitCode}}' $name))"
			start $name || exit 2
		elif [ $rc != 0 ]; then
			broken=$((broken + 1)); echo "$path round $i: harness step failed (rc=$rc): $out"
		fi
	done
	echo "$path: $iter rounds, init crashes=$crashes, harness failures=$broken"
	[ $crashes = 0 ] && [ $broken = 0 ] || fail=1
	$D rm -f $name >/dev/null
done
exit $fail
