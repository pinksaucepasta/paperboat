#!/bin/sh
# Runs the cli tests that skip unless they run as root (service units, protected install state, the runtime
# command and the local daemon's privileged paths), under sudo. Both main's CI and tools/ci-test.sh call this,
# so a root-only failure is never hidden by a skip. Extra arguments go to go test (for example -run X).
#   tools/cli-root-tests.sh [go test args]
set -eu
cd "$(dirname "$0")/../cli"
packages="./internal/hostruntime/service ./internal/hostruntime/hostinstall ./internal/hostruntimecmd ./internal/localdaemon
  ./internal/hostruntime/updated ./internal/hostruntime/sessionowner ./internal/hostruntime/releaseeligibility
  ./internal/hostruntime/environmentkey ./internal/machineguard ./internal/machineservices"
# Root gets its own build cache, so the job's cache stays owned by the runner user.
exec sudo env "PATH=$PATH" ${PB_TWO_USER_UIDS:+"PB_TWO_USER_UIDS=$PB_TWO_USER_UIDS"} GOTOOLCHAIN=local GOCACHE=/tmp/pb-root-gocache GOMODCACHE="$(go env GOMODCACHE)" \
  go test -count=1 -timeout 30m "$@" $packages
