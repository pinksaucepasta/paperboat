#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
manifest=$root/upstream/foundations.tsv
references=$root/../references

if [ "$#" -ne 0 ]; then
  echo "usage: $0" >&2
  exit 64
fi

for command in awk cmp git; do
  command -v "$command" >/dev/null 2>&1 || {
    echo "upstream foundations: $command is required" >&2
    exit 2
  }
done

test -f "$manifest" || {
  echo "upstream foundations: missing $manifest" >&2
  exit 2
}
test -d "$references" || {
  echo "upstream foundations: missing local references directory $references" >&2
  exit 2
}

manifest_value() {
  wanted_name=$1
  field=$2
  awk -F '\t' -v name="$wanted_name" -v field="$field" '
    $1 !~ /^#/ && NF >= field && $1 == name { print $field; exit }
  ' "$manifest"
}

valid_revision() {
  awk -v revision="$1" 'BEGIN {
    if (length(revision) == 40 && revision ~ /^[0-9a-f]+$/) exit 0
    exit 1
  }' </dev/null
}

short_revision() {
  printf '%s\n' "$1" | awk '{ print substr($0, 1, 12) }'
}

reference_for() {
  case "$1" in
  tailcat) printf '%s/tailcat\n' "$references" ;;
  tailscale-derp) printf '%s/tailscale\n' "$references" ;;
  *)
    echo "upstream foundations: unknown source $1" >&2
    exit 1
    ;;
  esac
}

source_count=0
while IFS="$(printf '\t')" read -r name repository module expected_module_version revision license_notice go_version; do
  case "$name" in
  ''|'#'*) continue ;;
  esac
  source_count=$((source_count + 1))
  test -n "$repository" && test -n "$module" && test -n "$expected_module_version" \
    && test -n "$revision" && test -n "$license_notice" && test -n "$go_version" || {
    echo "upstream foundations: incomplete manifest row for $name" >&2
    exit 1
  }
  valid_revision "$revision" || {
    echo "upstream foundations: $name revision is not a 40-character lowercase commit" >&2
    exit 1
  }
  case "$repository" in
  https://github.com/*/*.git) ;;
  *)
    echo "upstream foundations: $name repository is not an HTTPS GitHub origin" >&2
    exit 1
    ;;
  esac
  case "$expected_module_version" in
  *-"$(short_revision "$revision")") ;;
  *)
    echo "upstream foundations: $name module version does not identify $revision" >&2
    exit 1
    ;;
  esac
  test "$go_version" = 1.27.1 || {
    echo "upstream foundations: $name toolchain fact is $go_version, want 1.27.1" >&2
    exit 1
  }

  directory=$(reference_for "$name")
  test -d "$directory/.git" || {
    echo "upstream foundations: missing Git reference checkout for $name" >&2
    exit 1
  }
  git -C "$directory" cat-file -e "$revision^{commit}" 2>/dev/null || {
    echo "upstream foundations: $name reference lacks $revision" >&2
    exit 1
  }
  test -z "$(git -C "$directory" status --porcelain)" || {
    echo "upstream foundations: $name reference checkout is dirty" >&2
    exit 1
  }
  test -f "$directory/go.mod" || {
    echo "upstream foundations: $name reference has no go.mod" >&2
    exit 1
  }
  actual_module=$(awk '$1 == "module" { print $2; exit }' "$directory/go.mod")
  test "$actual_module" = "$module" || {
    echo "upstream foundations: $name declares $actual_module, want $module" >&2
    exit 1
  }
  actual_go_version=$(awk '$1 == "go" { print $2; exit }' "$directory/go.mod")
  test "$actual_go_version" = "$go_version" || {
    echo "upstream foundations: $name declares Go $actual_go_version, want $go_version" >&2
    exit 1
  }

  case "$license_notice" in
  BSD-3-Clause)
    notice="$root/upstream/licenses/tailcat-LICENSE.txt"
    test -f "$directory/LICENSE" && test -f "$notice" || exit 1
    cmp -s "$directory/LICENSE" "$notice" || {
      echo "upstream foundations: $name LICENSE differs from retained notice" >&2
      exit 1
    }
    ;;
  BSD-3-Clause+PATENTS)
    license="$root/upstream/licenses/tailscale-LICENSE.txt"
    patents="$root/upstream/licenses/tailscale-PATENTS.txt"
    test -f "$directory/LICENSE" && test -f "$directory/PATENTS" \
      && test -f "$license" && test -f "$patents" || exit 1
    cmp -s "$directory/LICENSE" "$license" || {
      echo "upstream foundations: $name LICENSE differs from retained notice" >&2
      exit 1
    }
    cmp -s "$directory/PATENTS" "$patents" || {
      echo "upstream foundations: $name PATENTS differs from retained notice" >&2
      exit 1
    }
    ;;
  *)
    echo "upstream foundations: unknown license notice $license_notice for $name" >&2
    exit 1
    ;;
  esac

  if [ "$name" = tailcat ]; then
    declared_tailscale=$(awk '$1 == "tailscale.com" { print $2; exit }' "$directory/go.mod")
    expected_tailscale=$(manifest_value tailscale-derp 4)
    test "$declared_tailscale" = "$expected_tailscale" || {
      echo "upstream foundations: Tailcat requires $declared_tailscale, want $expected_tailscale" >&2
      exit 1
    }
  fi
done < "$manifest"

test "$source_count" = 2 || {
  echo "upstream foundations: expected exactly two pinned sources, found $source_count" >&2
  exit 1
}
test "$(manifest_value tailcat 3)" = github.com/tailscale/tailcat || exit 1
test "$(manifest_value tailscale-derp 3)" = tailscale.com || exit 1

echo "upstream foundations: local pins, module facts, and notices verified"
