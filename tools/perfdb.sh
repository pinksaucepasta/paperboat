#!/usr/bin/env bash
# Prints PAPERBOAT_PERF_DATABASE_DSN for the server performance budgets (tools/gate.sh), creating the database
# in the isolated test PostgreSQL on oracle if needed. Never point it at production: the check seeds and
# truncates the database it is given. Usage: export PAPERBOAT_PERF_DATABASE_DSN=$(tools/perfdb.sh)
set -euo pipefail
db=paperboat_perf_test
pw=$(cat ~/.lane-b-test-pg-pw)
if ! sudo docker exec lane-b-test-pg psql -U postgres -Atc "SELECT 1 FROM pg_database WHERE datname='$db'" | grep -q 1; then
  sudo docker exec lane-b-test-pg psql -U postgres -qc "CREATE DATABASE $db" >&2
fi
echo "postgres://postgres:$pw@127.0.0.1:55433/$db?sslmode=disable"
