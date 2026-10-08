#!/usr/bin/env bash
set -euo pipefail

repository_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
publisher="$repository_root/tools/publish-tuf-origin.sh"
temporary=$(mktemp -d "${TMPDIR:-/tmp}/paperboat-publish-tuf.XXXXXX")
trap 'rm -rf "$temporary"' EXIT HUP INT TERM
release_root="$temporary/releases"
mkdir -p "$release_root/current/tuf/metadata" "$release_root/current/tuf/targets" "$release_root/staging"
printf 'previous current\n' > "$release_root/current/current.json"
printf 'previous timestamp\n' > "$release_root/current/tuf/metadata/timestamp.json"

# The production script deliberately pins its release root. Substitute only
# that guard for this local failure-atomicity test; no activation is attempted.
test_publisher="$temporary/publish-tuf-origin.sh"
sed "s|/opt/paperboat/releases|$release_root|g" "$publisher" > "$test_publisher"
chmod 0700 "$test_publisher"
test "$(awk 'NF { line=$0 } END { print line }' "$publisher")" = 'atomic_exchange "$live" "$next"'

write_current_manifest() {
  local path=$1
  local version=$2
  python3 - "$path" "$version" <<'PY'
import json
import pathlib
import sys

version = sys.argv[2]
assets = {
    "pb-darwin-arm64.pkg": ("darwin", "arm64", "pkg"),
    "pb-linux-amd64": ("linux", "amd64", "elf"),
    "pb-linux-arm64": ("linux", "arm64", "elf"),
    "pb-windows-amd64.exe": ("windows", "amd64", "pe"),
    "pb-windows-arm64.exe": ("windows", "arm64", "pe"),
}

body = {
    "schema": "paperboat.release-current/v1",
    "version": version,
    "repository": "pinksaucepasta/paperboat-cli",
    "assets": {
        name: {
            "platform": platform,
            "architecture": architecture,
            "format": format_,
            "url": f"https://github.com/pinksaucepasta/paperboat-cli/releases/download/{version}/{name}",
            "sha256": "0" * 64,
            "length": 1,
        }
        for name, (platform, architecture, format_) in assets.items()
    },
}
pathlib.Path(sys.argv[1]).write_text(json.dumps(body, separators=(",", ":")) + "\n")
PY
}

write_installers() {
  local root=$1 version=$2
  printf "#!/bin/sh\nbootstrap_version='%s'\nrepository=\${PAPERBOAT_GITHUB_REPOSITORY:-pinksaucepasta/paperboat-cli}\n" "$version" > "$root/install"
  printf "\$bootstrapVersion = '%s'\n\$repo = if (\$env:PAPERBOAT_GITHUB_REPOSITORY) { \$env:PAPERBOAT_GITHUB_REPOSITORY } else { 'pinksaucepasta/paperboat-cli' }\n" "$version" > "$root/windows"
}

write_matching_targets_metadata() {
  local current_path=$1
  local targets_path=$2
  python3 - "$current_path" "$targets_path" <<'PY'
import hashlib
import json
import pathlib
import sys

current = json.loads(pathlib.Path(sys.argv[1]).read_text())
targets = {}
for name, asset in current["assets"].items():
    manifest_sha256 = "1" * 64
    cohorts = []
    for wave_name, percentage, start_after_seconds, max_concurrent in (("canary", 1, 0, 1), ("early", 10, 3600, 10), ("general", 100, 7200, 100)):
        cohorts.append({
            "name": wave_name,
            "platform": asset["platform"],
            "architecture": asset["architecture"],
            "failure_domain": "*",
            "percentage": percentage,
            "start_after_seconds": start_after_seconds,
            "max_concurrent": max_concurrent,
        })
    deployment_plan = {
        "schema": "paperboat.release-deployment/v1",
        "version": current["version"],
        "manifest_sha256": manifest_sha256,
        "channel": "stable",
        "rollout_state": "active",
        "severity": "routine",
        "policy_revision": 1,
        "cohort_seed": "test-seed",
        "cohorts": cohorts,
        "canary": {"path": "/healthz", "expected_status": 200, "timeout_seconds": 10, "samples": 3, "require_edge": True, "require_connector": True, "require_route": True, "require_origin": True},
        "activation": {"drain_timeout_seconds": 30, "stability_window_seconds": 600, "stability_probe_interval_seconds": 30, "rollback_timeout_seconds": 60},
        "security_deferral": {"max_seconds": 604800, "requires_approval": False},
        "rollback": {"triggers": ["crash_loop", "watchdog_failure", "connector_authentication", "snapshot_apply", "edge_canary", "route_protocol", "state_migration", "readiness_regression"], "quarantine_seconds": 604800, "revoke_failed_release": True},
    }
    plan_bytes = (json.dumps(deployment_plan, separators=(",", ":"), ensure_ascii=True) + "\n").encode()
    index_target = {
        "component": "pb",
        "target_path": name,
        "asset_name": name,
        "repository": current["repository"],
        "download_url": asset["url"],
        "sha256": asset["sha256"],
        "length": asset["length"],
        "platform": asset["platform"],
        "architecture": asset["architecture"],
        "binary_format": asset["format"],
    }
    targets[name] = {
        "hashes": {"sha256": asset["sha256"]},
        "length": asset["length"],
        "custom": {
            "schema": "paperboat.tuf-asset/v1",
            "kind": "github-release-asset",
            "version": current["version"],
            "platform": asset["platform"],
            "architecture": asset["architecture"],
            "format": asset["format"],
            "asset_name": name,
            "repository": current["repository"],
            "url": asset["url"],
            "sha256": asset["sha256"],
            "length": asset["length"],
            "release_index": {
                "schema": "paperboat.release-index/v1",
                "release_id": "rel_" + current["version"],
                "version": current["version"],
                "channel": "stable",
                "severity": "routine",
                "created_at": "2026-08-31T12:00:00Z",
                "platform": asset["platform"],
                "architecture": asset["architecture"],
                "binary_format": asset["format"],
                "targets": [index_target],
                "hostd_api_min": 1,
                "hostd_api_max": 2,
                "runtime_api_min": 1,
                "runtime_api_max": 2,
                "rollout_policy_revision": 1,
                "supervisor_maintenance_required": False,
                "manifest_sha256": manifest_sha256,
                "deployment_plan_sha256": hashlib.sha256(plan_bytes).hexdigest(),
                "deployment_plan": deployment_plan,
            },
        },
    }
pathlib.Path(sys.argv[2]).write_text(json.dumps({"signed": {"_type": "targets", "targets": targets}}) + "\n")
PY
}

select_checksum_backend() {
  if command -v sha256sum >/dev/null 2>&1; then
    printf '%s\n' sha256sum
  elif command -v shasum >/dev/null 2>&1; then
    printf '%s\n' shasum
  else
    echo 'publisher test: sha256sum or shasum is required' >&2
    return 1
  fi
}

checksum_backend=$(select_checksum_backend)
checksum_sha256sum=$(command -v sha256sum || true)
checksum_shasum=$(command -v shasum || true)
if test -z "$checksum_sha256sum" && test -z "$checksum_shasum"; then
  echo 'publisher test: sha256sum or shasum is required' >&2
  exit 1
fi

run_checksum() {
  local backend=$1
  shift
  case "$backend" in
    sha256sum)
      test -n "$CHECKSUM_SHA256SUM_COMMAND"
      "$CHECKSUM_SHA256SUM_COMMAND" "$@"
      ;;
    shasum)
      test -n "$CHECKSUM_SHASUM_COMMAND"
      "$CHECKSUM_SHASUM_COMMAND" -a 256 "$@"
      ;;
    *)
      echo "publisher test: unsupported checksum backend: $backend" >&2
      return 1
      ;;
  esac
}

# Build command shims so both checksum branches are exercised even when the
# host only ships one of the two platform-specific command names. The shim
# delegates to the real command available on this host and only translates
# the shasum -a 256 argument shape.
checksum_test_bin="$temporary/checksum-bin"
mkdir -p "$checksum_test_bin"
if test -n "$checksum_sha256sum"; then
  cat > "$checksum_test_bin/sha256sum" <<EOF
#!/bin/sh
exec "$checksum_sha256sum" "\$@"
EOF
else
  cat > "$checksum_test_bin/sha256sum" <<EOF
#!/bin/sh
exec "$checksum_shasum" -a 256 "\$@"
EOF
fi
if test -n "$checksum_shasum"; then
  cat > "$checksum_test_bin/shasum" <<EOF
#!/bin/sh
exec "$checksum_shasum" "\$@"
EOF
else
  cat > "$checksum_test_bin/shasum" <<EOF
#!/bin/sh
set -eu
if test "\${1:-}" = -a && test "\${2:-}" = 256; then
  shift 2
fi
exec "$checksum_sha256sum" "\$@"
EOF
fi
chmod 0700 "$checksum_test_bin/sha256sum" "$checksum_test_bin/shasum"

# Keep the shim commands selected for the entire test. In particular, do not
# restore an absent host command after exercising the alternate branch: Git
# Bash on Windows normally provides sha256sum but not shasum, and doing so
# would make the later snapshot test call an empty command path.
CHECKSUM_SHA256SUM_COMMAND="$checksum_test_bin/sha256sum"
CHECKSUM_SHASUM_COMMAND="$checksum_test_bin/shasum"

run_test_publisher() {
  # The production publisher intentionally requires sha256sum on its Linux
  # deployment host. Supply the deterministic shim while this test simulates
  # a shasum-only development host, so its validation paths still execute
  # instead of passing early because a deployment-only command is absent.
  PATH="$checksum_test_bin:$PATH" "$test_publisher" "$@"
}

assert_isolated_checksum_backend() {
  local path=$1
  local expected=$2
  local unexpected
  if test "$expected" = sha256sum; then
    unexpected=shasum
  else
    unexpected=sha256sum
  fi

  test "$(PATH="$path" select_checksum_backend)" = "$expected"
  if PATH="$path" command -v "$unexpected" >/dev/null 2>&1; then
    echo "publisher test: isolated $expected host unexpectedly exposes $unexpected" >&2
    return 1
  fi
}

# Model both supported host environments explicitly. These directories each
# contain exactly one checksum command, so backend selection is tested without
# relying on which tools happen to be installed on the developer or runner.
sha256sum_only_path="$temporary/checksum-sha256sum-only"
shasum_only_path="$temporary/checksum-shasum-only"
mkdir -p "$sha256sum_only_path" "$shasum_only_path"
cp "$checksum_test_bin/sha256sum" "$sha256sum_only_path/sha256sum"
cp "$checksum_test_bin/shasum" "$shasum_only_path/shasum"
chmod 0700 "$sha256sum_only_path/sha256sum" "$shasum_only_path/shasum"
assert_isolated_checksum_backend "$sha256sum_only_path" sha256sum
assert_isolated_checksum_backend "$shasum_only_path" shasum

checksum_fixture="$temporary/checksum-fixture"
printf 'paperboat checksum fixture\n' > "$checksum_fixture"
CHECKSUM_SHA256SUM_COMMAND="$checksum_test_bin/sha256sum"
CHECKSUM_SHASUM_COMMAND="$checksum_test_bin/shasum"
checksum_sha256sum_digest=$(run_checksum sha256sum "$checksum_fixture" | awk '{print $1}')
checksum_shasum_digest=$(run_checksum shasum "$checksum_fixture" | awk '{print $1}')
test "$checksum_sha256sum_digest" = "$checksum_shasum_digest"
checksum_sha256sum_digest=$(printf 'paperboat checksum stream\n' | run_checksum sha256sum | awk '{print $1}')
checksum_shasum_digest=$(printf 'paperboat checksum stream\n' | run_checksum shasum | awk '{print $1}')
test "$checksum_sha256sum_digest" = "$checksum_shasum_digest"

snapshot_directory_with_backend() {
  local backend=$1
  local directory=$2
  (
    cd "$directory"
    while IFS= read -r -d '' file; do
      run_checksum "$backend" "$file"
    done < <(find . -type f -print0 | LC_ALL=C sort -z)
  )
}

run_native_checksum() {
  local backend=$1
  shift
  case "$backend" in
    sha256sum)
      "$checksum_sha256sum" "$@"
      ;;
    shasum)
      "$checksum_shasum" -a 256 "$@"
      ;;
    *)
      echo "publisher test: unsupported checksum backend: $backend" >&2
      return 1
      ;;
  esac
}

snapshot_directory_with_native_backend() {
  local backend=$1
  local directory=$2
  (
    cd "$directory"
    while IFS= read -r -d '' file; do
      run_native_checksum "$backend" "$file"
    done < <(find . -type f -print0 | LC_ALL=C sort -z)
  )
}

checksum_snapshot_fixture="$temporary/checksum-snapshot"
mkdir -p "$checksum_snapshot_fixture/nested directory"
printf 'first snapshot file\n' > "$checksum_snapshot_fixture/first"
printf 'second snapshot file\n' > "$checksum_snapshot_fixture/nested directory/second"
test "$(snapshot_directory_with_backend sha256sum "$checksum_snapshot_fixture")" = \
  "$(snapshot_directory_with_backend shasum "$checksum_snapshot_fixture")"

snapshot() {
  snapshot_directory_with_native_backend "$checksum_backend" "$release_root/current"
}
snapshot_directory() {
  snapshot_directory_with_native_backend "$checksum_backend" "$1"
}
live_version=2026.08.22.9
write_current_manifest "$temporary/live-current.json" "$live_version"
write_matching_targets_metadata "$temporary/live-current.json" "$release_root/current/tuf/metadata/targets.json"
cp "$release_root/current/tuf/metadata/targets.json" "$temporary/live-targets.json"
before=$(snapshot)

if run_test_publisher "$temporary/missing.tgz" "$release_root" 2026.08.22.23 "$(printf x | run_checksum "$checksum_backend" | awk '{print $1}')" >/dev/null 2>&1; then
  echo 'publisher accepted a missing bundle' >&2
  exit 1
fi
test "$before" = "$(snapshot)"

candidate="$temporary/candidate"
mkdir -p "$candidate/tuf/metadata" "$candidate/tuf/targets"
write_current_manifest "$candidate/current.json" wrong
write_installers "$candidate" 2026.08.22.23
for name in root targets snapshot timestamp; do printf x > "$candidate/tuf/metadata/$name.json"; done
bundle="$temporary/candidate.tgz"
tar -C "$candidate" -czf "$bundle" install windows tuf
digest=$(run_checksum "$checksum_backend" "$bundle" | awk '{print $1}')
if run_test_publisher "$bundle" "$release_root" 2026.08.22.23 "$digest" >/dev/null 2>&1; then
  echo 'publisher accepted invalid TUF metadata' >&2
  exit 1
fi
test "$before" = "$(snapshot)"

candidate_version=2026.08.22.10
write_current_manifest "$candidate/current.json" "$candidate_version"
write_installers "$candidate" "$candidate_version"
write_matching_targets_metadata "$candidate/current.json" "$candidate/tuf/metadata/targets.json"
python3 - "$candidate/tuf/metadata/targets.json" <<'PY'
import json
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
body = json.loads(path.read_text())
body["signed"]["targets"]["pb-linux-amd64"]["custom"]["version"] = "2026.08.22.24"
path.write_text(json.dumps(body) + "\n")
PY
mismatch_bundle="$temporary/mismatch.tgz"
tar -C "$candidate" -czf "$mismatch_bundle" install windows tuf
mismatch_digest=$(run_checksum "$checksum_backend" "$mismatch_bundle" | awk '{print $1}')
if run_test_publisher "$mismatch_bundle" "$release_root" "$candidate_version" "$mismatch_digest" >/dev/null 2>&1; then
  echo 'publisher accepted TUF version drift' >&2
  exit 1
fi
test "$before" = "$(snapshot)"

for field in manifest_sha256 deployment_plan_sha256 deployment_plan; do
  write_matching_targets_metadata "$candidate/current.json" "$candidate/tuf/metadata/targets.json"
  python3 - "$candidate/tuf/metadata/targets.json" "$field" <<'PY'
import json
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
field = sys.argv[2]
body = json.loads(path.read_text())
del body["signed"]["targets"]["pb-linux-arm64"]["custom"]["release_index"][field]
path.write_text(json.dumps(body) + "\n")
PY
  incomplete_bundle="$temporary/incomplete-$field.tgz"
  tar -C "$candidate" -czf "$incomplete_bundle" install windows tuf
  incomplete_digest=$(run_checksum "$checksum_backend" "$incomplete_bundle" | awk '{print $1}')
  if run_test_publisher "$incomplete_bundle" "$release_root" "$candidate_version" "$incomplete_digest" >/dev/null 2>&1; then
    echo "publisher accepted release metadata missing $field" >&2
    exit 1
  fi
  test "$before" = "$(snapshot)"
done

write_matching_targets_metadata "$candidate/current.json" "$candidate/tuf/metadata/targets.json"
# The signed release target set remains complete even when only some targets
# advance at this release version.
python3 - "$candidate/tuf/metadata/targets.json" <<'PY'
import json
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
document = json.loads(path.read_text())
del document["signed"]["targets"]["pb-linux-arm64"]
path.write_text(json.dumps(document) + "\n")
PY
omitted_bundle="$temporary/omitted-target.tgz"
tar -C "$candidate" -czf "$omitted_bundle" install windows tuf
omitted_digest=$(run_checksum "$checksum_backend" "$omitted_bundle" | awk '{print $1}')
if run_test_publisher "$omitted_bundle" "$release_root" "$candidate_version" "$omitted_digest" >/dev/null 2>&1; then
  echo 'publisher accepted targets with a canonical asset omitted' >&2
  exit 1
fi
test "$before" = "$(snapshot)"

# A selected target URL remains bound to its repository, requested version,
# and canonical asset name.
write_matching_targets_metadata "$candidate/current.json" "$candidate/tuf/metadata/targets.json"
python3 - "$candidate/tuf/metadata/targets.json" <<'PY'
import json
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
document = json.loads(path.read_text())
target = document["signed"]["targets"]["pb-linux-amd64"]
forged_url = "https://github.com/pinksaucepasta/paperboat-cli/releases/download/2026.08.22.24/pb-linux-amd64"
target["custom"]["url"] = forged_url
target["custom"]["release_index"]["targets"][0]["download_url"] = forged_url
path.write_text(json.dumps(document) + "\n")
PY
forged_url_bundle="$temporary/forged-url.tgz"
tar -C "$candidate" -czf "$forged_url_bundle" install windows tuf
forged_url_digest=$(run_checksum "$checksum_backend" "$forged_url_bundle" | awk '{print $1}')
if run_test_publisher "$forged_url_bundle" "$release_root" "$candidate_version" "$forged_url_digest" >/dev/null 2>&1; then
  echo 'publisher accepted a forged selected-target URL' >&2
  exit 1
fi
test "$before" = "$(snapshot)"

# The requested version must select at least one target; installer pins alone
# cannot advance the publication.
cp "$temporary/live-targets.json" "$candidate/tuf/metadata/targets.json"
no_selected_bundle="$temporary/no-selected.tgz"
tar -C "$candidate" -czf "$no_selected_bundle" install windows tuf
no_selected_digest=$(run_checksum "$checksum_backend" "$no_selected_bundle" | awk '{print $1}')
if run_test_publisher "$no_selected_bundle" "$release_root" "$candidate_version" "$no_selected_digest" >/dev/null 2>&1; then
  echo 'publisher accepted targets with no artifact at the requested release version' >&2
  exit 1
fi
test "$before" = "$(snapshot)"

# A selected target must advance beyond the version already served for it.
same_version=$live_version
write_installers "$candidate" "$same_version"
cp "$temporary/live-targets.json" "$candidate/tuf/metadata/targets.json"
stale_bundle="$temporary/stale-version.tgz"
tar -C "$candidate" -czf "$stale_bundle" install windows tuf
stale_digest=$(run_checksum "$checksum_backend" "$stale_bundle" | awk '{print $1}')
if run_test_publisher "$stale_bundle" "$release_root" "$same_version" "$stale_digest" >/dev/null 2>&1; then
  echo 'publisher accepted a selected target without a strictly newer version' >&2
  exit 1
fi
test "$before" = "$(snapshot)"

# Alter one omitted artifact identity consistently across its TUF target and
# release index. It is still rejected because omitted entries must be exact.
write_installers "$candidate" "$candidate_version"
write_matching_targets_metadata "$candidate/current.json" "$candidate/tuf/metadata/targets.json"
python3 - "$candidate/tuf/metadata/targets.json" "$temporary/live-targets.json" <<'PY'
import json
import pathlib
import sys

candidate_path = pathlib.Path(sys.argv[1])
live = json.loads(pathlib.Path(sys.argv[2]).read_text())
candidate = json.loads(candidate_path.read_text())
for name in ("pb-linux-arm64", "pb-windows-arm64.exe"):
    candidate["signed"]["targets"][name] = live["signed"]["targets"][name]
omitted = candidate["signed"]["targets"]["pb-linux-arm64"]
omitted["hashes"]["sha256"] = "f" * 64
omitted["custom"]["sha256"] = "f" * 64
omitted["custom"]["release_index"]["targets"][0]["sha256"] = "f" * 64
candidate_path.write_text(json.dumps(candidate) + "\n")
PY
tampered_omitted_bundle="$temporary/tampered-omitted.tgz"
tar -C "$candidate" -czf "$tampered_omitted_bundle" install windows tuf
tampered_omitted_digest=$(run_checksum "$checksum_backend" "$tampered_omitted_bundle" | awk '{print $1}')
if run_test_publisher "$tampered_omitted_bundle" "$release_root" "$candidate_version" "$tampered_omitted_digest" >/dev/null 2>&1; then
  echo 'publisher accepted changed metadata for an omitted target' >&2
  exit 1
fi
test "$before" = "$(snapshot)"

# A new target may not forge a repository distinct from the retained entries.
write_matching_targets_metadata "$candidate/current.json" "$candidate/tuf/metadata/targets.json"
python3 - "$candidate/tuf/metadata/targets.json" "$temporary/live-targets.json" <<'PY'
import json
import pathlib
import sys

candidate_path = pathlib.Path(sys.argv[1])
live = json.loads(pathlib.Path(sys.argv[2]).read_text())
candidate = json.loads(candidate_path.read_text())
for name in ("pb-linux-arm64", "pb-windows-arm64.exe"):
    candidate["signed"]["targets"][name] = live["signed"]["targets"][name]
name = "pb-linux-amd64"
target = candidate["signed"]["targets"][name]
repository = "attacker.invalid/paperboat"
url = f"https://github.com/{repository}/releases/download/{target['custom']['version']}/{name}"
target["custom"]["repository"] = repository
target["custom"]["url"] = url
target["custom"]["release_index"]["targets"][0]["repository"] = repository
target["custom"]["release_index"]["targets"][0]["download_url"] = url
candidate_path.write_text(json.dumps(candidate) + "\n")
PY
forged_repo_bundle="$temporary/forged-repository.tgz"
tar -C "$candidate" -czf "$forged_repo_bundle" install windows tuf
forged_repo_digest=$(run_checksum "$checksum_backend" "$forged_repo_bundle" | awk '{print $1}')
if run_test_publisher "$forged_repo_bundle" "$release_root" "$candidate_version" "$forged_repo_digest" >/dev/null 2>&1; then
  echo 'publisher accepted a changed selected-target repository' >&2
  exit 1
fi
test "$before" = "$(snapshot)"

write_installers "$candidate" "$candidate_version"
write_matching_targets_metadata "$candidate/current.json" "$candidate/tuf/metadata/targets.json"
# Retain the two omitted targets from the active metadata in the valid bundle.
python3 - "$candidate/tuf/metadata/targets.json" "$temporary/live-targets.json" <<'PY'
import json
import pathlib
import sys

candidate_path = pathlib.Path(sys.argv[1])
live = json.loads(pathlib.Path(sys.argv[2]).read_text())
candidate = json.loads(candidate_path.read_text())
for name in ("pb-linux-arm64", "pb-windows-arm64.exe"):
    candidate["signed"]["targets"][name] = live["signed"]["targets"][name]
candidate_path.write_text(json.dumps(candidate) + "\n")
PY
bundle="$temporary/candidate.tgz"
tar -C "$candidate" -czf "$bundle" install windows tuf
digest=$(run_checksum "$checksum_backend" "$bundle" | awk '{print $1}')

# renameat2(RENAME_EXCHANGE) is a Linux deployment requirement. Exercise the
# success path there with a fake Docker CLI that proves the parent RO mount;
# other development hosts still execute the two pre-activation failure tests.
if [ "$(uname -s)" = Linux ]; then
  mkdir -p "$temporary/bin"
  cat > "$temporary/bin/docker" <<'EOF'
#!/bin/sh
set -eu
case "${1:-}" in
  ps)
    test "${2:-}" = -q
    echo test-container
    ;;
  inspect)
    case "${PAPERBOAT_TEST_DOCKER_MODE:?}" in
      good)
        printf '[{"Mounts":[{"Type":"bind","Source":"%s","Destination":"/srv/paperboat-releases","RW":false}],"Config":{"Env":["PAPERBOAT_RELEASE_DIRECTORY=/srv/paperboat-releases/current"]}}]\n' "$PAPERBOAT_TEST_RELEASE_ROOT"
        ;;
      wrong-env)
        printf '[{"Mounts":[{"Type":"bind","Source":"%s","Destination":"/srv/paperboat-releases","RW":false}],"Config":{"Env":["PAPERBOAT_RELEASE_DIRECTORY=/srv/other"]}}]\n' "$PAPERBOAT_TEST_RELEASE_ROOT"
        ;;
      split)
        printf '[{"Mounts":[{"Type":"bind","Source":"%s","Destination":"/srv/paperboat-releases","RW":false}],"Config":{"Env":[]}}, {"Mounts":[],"Config":{"Env":["PAPERBOAT_RELEASE_DIRECTORY=/srv/paperboat-releases/current"]}}]\n' "$PAPERBOAT_TEST_RELEASE_ROOT"
        ;;
      stale)
        printf '[{"Mounts":[{"Type":"bind","Source":"%s/current","Destination":"/srv/paperboat-releases","RW":false}],"Config":{"Env":["PAPERBOAT_RELEASE_DIRECTORY=/srv/paperboat-releases/current"]}}]\n' "$PAPERBOAT_TEST_RELEASE_ROOT"
        ;;
      *) exit 2 ;;
    esac
    ;;
  *) exit 2 ;;
esac
EOF
  cat > "$temporary/bin/chown" <<'EOF'
#!/bin/sh
exit 0
EOF
  chmod 0700 "$temporary/bin/docker" "$temporary/bin/chown"

  expected="$temporary/expected"
  cp -R "$candidate" "$expected"
  rm -f "$expected/current.json"
  expected_candidate=$(snapshot_directory "$expected")
  PATH="$temporary/bin:$PATH" PAPERBOAT_TEST_DOCKER_MODE=good PAPERBOAT_TEST_RELEASE_ROOT="$release_root" run_test_publisher "$bundle" "$release_root" "$candidate_version" "$digest"
  set -- "$release_root"/staging/activation-*
  test "$#" -eq 1 && test -d "$1"
  transaction=$1
  test "$expected_candidate" = "$(snapshot)"
  test "$before" = "$(snapshot_directory "$transaction/next")"
  python3 - "$release_root/current/tuf/metadata/targets.json" "$temporary/live-targets.json" <<'PY'
import json
import pathlib
import sys

active = json.loads(pathlib.Path(sys.argv[1]).read_text())
live = json.loads(pathlib.Path(sys.argv[2]).read_text())
active_targets = active["signed"]["targets"]
live_targets = live["signed"]["targets"]
for name in ("pb-linux-arm64", "pb-windows-arm64.exe"):
    if active_targets[name] != live_targets[name]:
        raise SystemExit(f"omitted target metadata changed after activation: {name}")
for name in ("pb-darwin-arm64.pkg", "pb-linux-amd64", "pb-windows-amd64.exe"):
    if active_targets[name]["custom"]["version"] != "2026.08.22.10":
        raise SystemExit(f"selected target did not advance: {name}")
PY

  next="$temporary/next"
  mkdir -p "$next/tuf/metadata" "$next/tuf/targets"
  write_current_manifest "$next/current.json" 2026.08.22.24
  write_installers "$next" 2026.08.22.24
  for name in root targets snapshot timestamp; do printf x > "$next/tuf/metadata/$name.json"; done
  write_matching_targets_metadata "$next/current.json" "$next/tuf/metadata/targets.json"
  next_bundle="$temporary/next.tgz"
  tar -C "$next" -czf "$next_bundle" install windows tuf
  next_digest=$(run_checksum "$checksum_backend" "$next_bundle" | awk '{print $1}')
  live_before_wrong_env=$(snapshot)
  if PATH="$temporary/bin:$PATH" PAPERBOAT_TEST_DOCKER_MODE=wrong-env PAPERBOAT_TEST_RELEASE_ROOT="$release_root" run_test_publisher "$next_bundle" "$release_root" 2026.08.22.24 "$next_digest" >/dev/null 2>&1; then
    echo 'publisher accepted a release mount with the wrong runtime directory' >&2
    exit 1
  fi
  test "$live_before_wrong_env" = "$(snapshot)"
  if PATH="$temporary/bin:$PATH" PAPERBOAT_TEST_DOCKER_MODE=split PAPERBOAT_TEST_RELEASE_ROOT="$release_root" run_test_publisher "$next_bundle" "$release_root" 2026.08.22.24 "$next_digest" >/dev/null 2>&1; then
    echo 'publisher accepted split release mount and runtime configuration containers' >&2
    exit 1
  fi
  test "$live_before_wrong_env" = "$(snapshot)"
  live_before_stale=$(snapshot)
  if PATH="$temporary/bin:$PATH" PAPERBOAT_TEST_DOCKER_MODE=stale PAPERBOAT_TEST_RELEASE_ROOT="$release_root" run_test_publisher "$next_bundle" "$release_root" 2026.08.22.24 "$next_digest" >/dev/null 2>&1; then
    echo 'publisher accepted a stale current-directory bind mount' >&2
    exit 1
  fi
  test ! -e "$transaction"
  test "$live_before_stale" = "$(snapshot)"
fi
