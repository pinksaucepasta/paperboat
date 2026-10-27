#!/bin/sh
# usage: export-config.sh <verified dump> <outdir>
# Restores the dump into a scratch container and writes the rows a wipe must keep (config tables, platform
# certificates) and the fixture rows (paid team, owner, active members) as COPY files. Prod is not touched.
. "$(dirname "$0")/lib.sh"
dump=$1; out=$2
name=$(scratch_start)
trap 'docker rm -f "$name" >/dev/null 2>&1' EXIT
docker cp "$dump" "$name:/tmp/export.dump"
docker exec "$name" pg_restore -U postgres -d paperboat --no-owner --exit-on-error /tmp/export.dump
rm -rf "$out"
export_sets "$name" paperboat "$out/config" config_sets
export_sets "$name" paperboat "$out/fixture" fixture_sets
echo "EXPORTED $out"
