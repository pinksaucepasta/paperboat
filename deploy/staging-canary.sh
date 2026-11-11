#!/usr/bin/env bash
# R-04: one canary pass on staging (exec both ways and ssh between pb-stg-a and pb-stg-b on anvit); exit 1 on any failure.
# Staging has no edge, so previews, tunnels and file sends are not part of it (deploy/README.md). Run from oracle.
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
host=${STAGING_HOST:-ubuntu@anvit}
on() { echo "ssh -o BatchMode=yes $host sudo -n docker exec -i -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 $1"; }
RUN_A=$(on pb-stg-a) RUN_B=$(on pb-stg-b) CANARY_A=stg-a CANARY_B=stg-b \
  PROBE_STEPS="exec_a_to_b exec_b_to_a ssh_a_to_b" "$here/../tools/probe/probe.sh"
