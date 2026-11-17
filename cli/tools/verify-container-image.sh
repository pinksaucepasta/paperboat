#!/bin/sh
set -eu

dockerfile=deploy/container/Dockerfile
compose=deploy/self-hosted/compose.yaml

test -f "$dockerfile"
test -f "$compose"
test ! -e deploy/container/entrypoint.sh

test "$(grep -c '^FROM .*@sha256:[0-9a-f]\{64\}' "$dockerfile")" -eq 2
# The image builds from the repository root: cli needs the relay and shared modules next to it.
grep -F 'COPY cli/go.mod cli/go.sum cli/' "$dockerfile" >/dev/null
grep -F 'COPY relay relay' "$dockerfile" >/dev/null
grep -F 'COPY shared shared' "$dockerfile" >/dev/null
grep -F 'useradd --uid 10001' "$dockerfile" >/dev/null
grep -F 'COPY --from=build /out/pb /usr/local/bin/pb' "$dockerfile" >/dev/null
grep -F 'ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/pb", "daemon", "--foreground"]' "$dockerfile" >/dev/null
if grep -E '^CMD' "$dockerfile" >/dev/null; then
  echo "container image must not accept an arbitrary command in place of hostd" >&2
  exit 1
fi

grep -F 'read_only: true' "$compose" >/dev/null
grep -F 'paperboat-state:/var/lib/paperboat' "$compose" >/dev/null
grep -F 'paperboat-workspace:/workspace' "$compose" >/dev/null
grep -F '/run:mode=755,nosuid,nodev' "$compose" >/dev/null
if grep -E '^    ports:' "$compose" >/dev/null; then
  echo "container deployment must not publish Paperboat ports" >&2
  exit 1
fi
# A first start needs only the enrollment token: no machine ID or release repository is required.
if grep -E 'PAPERBOAT_MACHINE_ID|PAPERBOAT_RELEASE_REPOSITORY' "$compose" >/dev/null; then
  echo "compose must not require the machine ID or release repository" >&2
  exit 1
fi

echo "container image: valid"
