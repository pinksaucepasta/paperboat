#!/bin/sh
set -eu

dockerfile=deploy/container/Dockerfile
entrypoint=deploy/container/entrypoint.sh
launcher=deploy/container/pb-launcher.sh
compose=deploy/self-hosted/compose.yaml

test -f "$dockerfile"
test -f "$entrypoint"
test -f "$launcher"
test -f "$compose"
sh -n "$entrypoint"
sh -n "$launcher"

test "$(grep -c '^FROM .*@sha256:[0-9a-f]\{64\}' "$dockerfile")" -eq 2
grep -F 'paperboat-hostd' "$dockerfile" >/dev/null
grep -F 'paperboat-updated' "$dockerfile" >/dev/null
grep -F 'paperboat-runtime.bootstrap' "$dockerfile" >/dev/null
grep -F 'useradd --uid 10001' "$dockerfile" >/dev/null
grep -F 'COPY deploy/container/pb-launcher.sh /usr/local/bin/pb' "$dockerfile" >/dev/null
if grep -E '^CMD' "$dockerfile" >/dev/null; then
  echo "container image must not accept an arbitrary command in place of hostd" >&2
  exit 1
fi
grep -F 'PAPERBOAT_RELEASE_REPOSITORY must use https' "$entrypoint" >/dev/null
grep -F 'PAPERBOAT_RUNTIME_CURRENT="$release_root/runtime-current/paperboat-runtime"' "$entrypoint" >/dev/null
grep -F 'PAPERBOAT_UPDATE_STATE_ROOT="$updated_root"' "$entrypoint" >/dev/null
grep -F 'PAPERBOAT_HOSTD_TOKEN_FILE="$token_root/token"' "$entrypoint" >/dev/null
grep -F 'export HOME=/workspace' "$entrypoint" >/dev/null
grep -F '/usr/bin/setpriv --reuid="$runtime_uid" --regid="$runtime_gid" --init-groups' "$entrypoint" >/dev/null
grep -F 'paperboat-hostd daemon __runtime-hostd &' "$entrypoint" >/dev/null
grep -F 'paperboat-updated daemon __runtime-updated' "$entrypoint" >/dev/null
grep -F 'target=/var/lib/paperboat/releases/cli-current/pb' "$launcher" >/dev/null

grep -F 'read_only: true' "$compose" >/dev/null
grep -F 'paperboat-state:/var/lib/paperboat' "$compose" >/dev/null
grep -F 'paperboat-workspace:/workspace' "$compose" >/dev/null
grep -F '/run:mode=755,nosuid,nodev' "$compose" >/dev/null
if grep -E '^    ports:' "$compose" >/dev/null; then
  echo "container deployment must not publish Paperboat ports" >&2
  exit 1
fi

echo "container image: valid"
