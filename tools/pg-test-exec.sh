#!/usr/bin/env bash
# `go test -exec` wrapper: each package's test binary runs on its own fresh, empty database.
#   PAPERBOAT_TEST_DATABASE_DSN='postgres://u:p@host:5432/lane_test?sslmode=disable' \
#     go test -exec "$(git rev-parse --show-toplevel)/tools/pg-test-exec.sh" ./...
# The DSN names a database called lane_test on a server where the role may create databases; every package gets
# lane_<package path>_test on the same server (the *_test name is what ValidateIsolatedTestDSN requires), dropped and
# recreated first, so no package sees rows another left behind while go test still runs packages in parallel.
# Used by CI (server-ci.yml, test.yml) and tools/remote-test.sh. Without the variable it just runs the binary.
set -euo pipefail
base=${PAPERBOAT_TEST_DATABASE_DSN:-}
[ -n "$base" ] || exec "$@"
# A DSN naming any other database would be cut wrongly below and every package would share one database.
case $base in */lane_test\?*) ;; *) echo "pg-test-exec: PAPERBOAT_TEST_DATABASE_DSN must name the database lane_test followed by ?options" >&2; exit 1 ;; esac
root=$PWD
while [ ! -f "$root/go.mod" ]; do root=$(dirname "$root"); done # go test runs the binary in its package directory
rel=${PWD#"$root"}; rel=${rel#/}; rel=${rel:-root}
name=$(printf %s "$rel" | tr -c 'A-Za-z0-9' _)
db="lane_${name}_test"
# PostgreSQL silently cuts identifiers at 63 bytes: keep long package paths distinct.
[ ${#db} -le 63 ] || db="lane_${name:0:36}_$(printf %s "$rel" | md5sum | cut -c1-8)_test"
PGOPTIONS='-c client_min_messages=warning' psql "${base%%lane_test*}postgres?sslmode=disable" -qv ON_ERROR_STOP=1 \
  -c "drop database if exists \"$db\" with (force)" -c "create database \"$db\"" ||
  { echo "pg-test-exec: cannot create database $db for $rel" >&2; exit 1; }
PAPERBOAT_TEST_DATABASE_DSN="${base/lane_test\?/$db?}" exec "$@"
