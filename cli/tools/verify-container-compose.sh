#!/bin/sh
# Validate Compose syntax and interpolation without talking to a Docker daemon.
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root"

if docker compose version >/dev/null 2>&1; then
  compose() { docker compose "$@"; }
elif command -v docker-compose >/dev/null 2>&1; then
  compose() { docker-compose "$@"; }
else
  echo "container compose validation: Docker Compose is required" >&2
  exit 1
fi

run_compose() {
  PAPERBOAT_IMAGE=example.invalid/paperboat:test \
  PAPERBOAT_RELEASE_REPOSITORY=https://releases.paperboat.test \
  PAPERBOAT_MACHINE_ID=mch_container_test \
  PAPERBOAT_CONTROL_URL=https://api.paperboat.test \
  PAPERBOAT_ENROLLMENT_CREDENTIAL=test-enrollment \
  compose -f "$1" config >/dev/null
}

run_compose deploy/self-hosted/compose.yaml
echo "container compose: valid"
