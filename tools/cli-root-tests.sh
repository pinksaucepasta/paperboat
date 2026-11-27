#!/bin/sh
# Runs the cli tests that skip unless they run as root (service units, protected install state, the runtime
# command, the local daemon's privileged paths, the updater, the machine guard, session ownership and the
# system credential), under sudo. Both main's CI and tools/ci-test.sh call this,
# so a root-only failure is never hidden by a skip. Extra arguments go to go test (for example -run X).
#   tools/cli-root-tests.sh [go test args]
# Machine guard state the run creates (units, state, nft table, CA trust files) is removed afterwards, on
# success, failure and interruption, so a later `pb install` on the same host starts clean. A host with a
# machine guard already installed is refused: the tests would share its trust path.
set -eu
cd "$(dirname "$0")/../cli"
packages="./internal/hostruntime/service ./internal/hostruntime/hostinstall ./internal/hostruntimecmd ./internal/localdaemon
  ./internal/hostruntime/updated ./internal/hostruntime/releaseeligibility ./internal/hostruntime/sessionowner
  ./internal/hostruntime/environmentkey ./internal/machineguard ./internal/machineservices"
# The two-OS-user gateway checks (F23-01, F23-02) run processes under these uids; they need no account.
# Root gets its own build cache, so the job's cache stays owned by the runner user.
# The Linux pool's user-scoped Go 1.27.4 (tools/remote-test.sh does the same; the image's Go is older).
export PATH=$HOME/.local/go1.27.4/bin:$PATH

guard_paths="/etc/systemd/system/paperboat-machineguard.service /etc/systemd/system/paperboat-machineguard-deny.service
  /var/lib/paperboat-machineguard /run/paperboat-machineguard /usr/local/libexec/paperboat-machineguard"
trust_glob=/usr/local/share/ca-certificates/paperboat-machineguard-
for path in $guard_paths; do
  if sudo test -e "$path"; then
    echo "cli-root-tests: $path exists; run on a host without a Paperboat machine guard (pb uninstall first)" >&2
    exit 1
  fi
done
# Install state the hostinstall tests create when the host had none is removed too.
install_created=""
for path in /usr/local/libexec/paperboat /var/lib/paperboat-installer; do
  if ! sudo test -e "$path"; then install_created="$install_created $path"; fi
done
trust_before=$(sudo sh -c "ls -1 ${trust_glob}*.crt 2>/dev/null" || true)

cleanup() {
  status=$?
  set +e
  if [ "$(uname -s)" = Linux ]; then
    sudo systemctl disable --now paperboat-machineguard.service paperboat-machineguard-deny.service 2>/dev/null
    sudo rm -rf $guard_paths $install_created
    sudo systemctl daemon-reload 2>/dev/null
    sudo nft delete table inet paperboat_machineguard 2>/dev/null
    changed=0
    for file in $(sudo sh -c "ls -1 ${trust_glob}*.crt 2>/dev/null" || true); do
      case "
$trust_before
" in *"
$file
"*) ;; *) sudo rm -f "$file"; changed=1 ;; esac
    done
    if [ "$changed" = 1 ]; then sudo update-ca-certificates --fresh >/dev/null 2>&1; fi
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT TERM

sudo env "PATH=$PATH" GOTOOLCHAIN=local GOCACHE=/tmp/pb-root-gocache GOMODCACHE="$(go env GOMODCACHE)" \
  PB_TWO_USER_UIDS=64001,64002 \
  go test -count=1 -timeout 30m "$@" $packages
