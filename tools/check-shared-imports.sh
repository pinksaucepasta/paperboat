#!/usr/bin/env bash
# Shared is embeddable: no CLI or relay module may enter its production or test import closure.
set -euo pipefail
cd "$(dirname "$0")/../shared"
for target in linux darwin windows; do
  modules=$(GOOS="$target" CGO_ENABLED=0 go list -deps -test -f '{{if .Module}}{{.Module.Path}}{{end}}' ./...)
  while IFS= read -r module; do
    case "$module" in
      github.com/pinksaucepasta/paperboat|github.com/pinksaucepasta/paperboat/cli|github.com/pinksaucepasta/paperboat/relay)
        echo "shared import boundary failed on $target: $module" >&2
        exit 1
        ;;
    esac
  done <<< "$modules"
  echo "shared import boundary: $target PASS"
done
