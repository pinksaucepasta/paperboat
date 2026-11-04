#!/usr/bin/env bash
# Dispatch .github/workflows/macos-device-checks.yml with a fresh enrollment token.
# usage: tools/macos-runner.sh [--ref REF] [--os macos-15|macos-14] [--wait]
# The token secret is deleted on exit; without --wait the job may not have read it yet, so use --wait.
set -euo pipefail
ref=$(git rev-parse --abbrev-ref HEAD); os=macos-15; wait=0
while [ $# -gt 0 ]; do
  case $1 in --ref) ref=$2; shift 2;; --os) os=$2; shift 2;; --wait) wait=1; shift;; *) echo "unknown $1" >&2; exit 2;; esac
done
PW=/home/ubuntu/.pbmac/pw
out=$(mktemp); trap 'rm -f "$out"; gh secret delete PB_MAC_ENROLL_TOKEN >/dev/null 2>&1 || true' EXIT
CHROME=/home/ubuntu/.cache/ms-playwright/chromium_headless_shell-1248/chrome-headless-shell-linux-arm64/chrome-headless-shell \
  node "$PW/mint.mjs" "$PW/s2.json" "$out" >/dev/null
python3 -c 'import json,sys; sys.stdout.write(json.load(open(sys.argv[1]))["data"]["bootstrap_token"])' "$out" |
  gh secret set PB_MAC_ENROLL_TOKEN
start=$(date -u +%Y-%m-%dT%H:%M:%SZ)
gh workflow run macos-device-checks.yml --ref "$ref" -f ref="$ref" -f os="$os"
sleep 10
id=$(gh run list --workflow macos-device-checks.yml --branch "$ref" --created ">=$start" --limit 1 --json databaseId -q '.[0].databaseId')
echo "run: $(gh run view "$id" --json url -q .url)"
[ $wait = 1 ] || exit 0
gh run watch "$id" --interval 20 >/dev/null || true
d=$(mktemp -d); gh run download "$id" -D "$d" >/dev/null 2>&1 || true
cat "$d"/*/results.jsonl 2>/dev/null || echo "no results artifact"
gh run view "$id" --json startedAt,createdAt,conclusion -q '"conclusion=\(.conclusion) created=\(.createdAt) updated=\(.startedAt)"'
