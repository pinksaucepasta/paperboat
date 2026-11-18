#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 4 ]]; then
  echo "usage: publish-tuf-origin.sh STAGED_BUNDLE RELEASE_ROOT VERSION EXPECTED_SHA256" >&2
  exit 2
fi

bundle=$1
release_root=$2
version=$3
expected_sha=$4

[[ "$release_root" == /opt/paperboat/releases ]] || { echo "unexpected release root" >&2; exit 1; }
[[ "$version" =~ ^20[0-9]{2}\.[0-9]{2}\.[0-9]{2}\.(0|[1-9][0-9]*)$ ]] || { echo "invalid release version" >&2; exit 1; }
[[ "$expected_sha" =~ ^[a-f0-9]{64}$ ]] || { echo "invalid bundle digest" >&2; exit 1; }
[[ -f "$bundle" && ! -L "$bundle" ]] || { echo "staged bundle is unavailable" >&2; exit 1; }
actual_sha=$(sha256sum "$bundle" | awk '{print $1}')
[[ "$actual_sha" == "$expected_sha" ]] || { echo "staged bundle digest mismatch" >&2; exit 1; }

verify_live_mount_contract() {
  local release_root=$1
  local live="$release_root/current"
  local staging="$release_root/staging"
  [[ -d "$release_root" && ! -L "$release_root" ]] || { echo "release root is unavailable" >&2; return 1; }
  [[ -d "$live" && ! -L "$live" ]] || { echo "live release is unavailable" >&2; return 1; }
  [[ -d "$staging" && ! -L "$staging" ]] || { echo "release staging directory is unavailable" >&2; return 1; }
  if [[ "$live_state" != fresh ]]; then
    [[ -d "$live/tuf/metadata" && ! -L "$live/tuf/metadata" ]] || { echo "live TUF metadata is unavailable" >&2; return 1; }
    [[ -d "$live/tuf/targets" && ! -L "$live/tuf/targets" ]] || { echo "live TUF targets are unavailable" >&2; return 1; }
  fi
  mapfile -t containers < <(docker ps -q)
  ((${#containers[@]} > 0)) || { echo "no running containers are available to verify the release mount" >&2; return 1; }
  local inspect
  inspect=$(mktemp) || return 1
  if ! docker inspect "${containers[@]}" > "$inspect"; then
    rm -f -- "$inspect"
    return 1
  fi
  if ! python3 - "$release_root" "$inspect" <<'PY'
import json
import sys

containers = json.load(open(sys.argv[2], encoding="utf-8"))
parent_source = sys.argv[1]
old_source = parent_source + "/current"
destination = "/srv/paperboat-releases"
runtime = destination + "/current"
ready = False
for container in containers:
    mounts = container.get("Mounts", [])
    if any(mount.get("Source") == old_source for mount in mounts):
        raise SystemExit("a running container still bind-mounts the old current release directory")
    parent_mount = any(
        mount.get("Source") == parent_source
        and mount.get("Destination") == destination
        and mount.get("Type") == "bind"
        and mount.get("RW") is False
        for mount in mounts
    )
    runtime_env = runtime in {
        value.split("=", 1)[1]
        for value in container.get("Config", {}).get("Env", [])
        if value.startswith("PAPERBOAT_RELEASE_DIRECTORY=")
    }
    ready = ready or (parent_mount and runtime_env)
if not ready:
    raise SystemExit("no single running container exposes the read-only releases parent mount and current runtime directory")
PY
  then
    rm -f -- "$inspect"
    return 1
  fi
  rm -f -- "$inspect"
}

atomic_exchange() {
  python3 - "$1" "$2" <<'PY'
import ctypes
import os
import sys

libc = ctypes.CDLL(None, use_errno=True)
renameat2 = getattr(libc, "renameat2", None)
if renameat2 is None:
    raise SystemExit("renameat2 is unavailable")
renameat2.argtypes = [ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_char_p, ctypes.c_uint]
renameat2.restype = ctypes.c_int
if renameat2(-100, os.fsencode(sys.argv[1]), -100, os.fsencode(sys.argv[2]), 2) != 0:
    error = ctypes.get_errno()
    raise OSError(error, os.strerror(error))
PY
}

live="$release_root/current"
staging="$release_root/staging"
[[ -d "$live" && ! -L "$live" ]] || { echo "live release is unavailable" >&2; exit 1; }
[[ -d "$staging" && ! -L "$staging" ]] || { echo "release staging directory is unavailable" >&2; exit 1; }

# Serialize validation, staging cleanup and exchange against the same live tree.
# Keep this descriptor open through the final exchange; never unlink the lock.
exec 9>"$release_root/.publish-tuf-origin.lock"
flock -x 9

# One explicit initial state: no metadata, and only empty,
# regular TUF directories if deployment has created them. Partial metadata
# is an initialized release that must pass every ordinary predecessor check.
live_state=$(python3 - "$live" <<'PY_STATE'
from pathlib import Path
import sys
live = Path(sys.argv[1])
tuf = live / "tuf"
for directory in (tuf, tuf / "metadata", tuf / "targets"):
    if directory.is_symlink() or (directory.exists() and not directory.is_dir()):
        raise SystemExit("live TUF directory is unsafe; restore regular directories before publishing")
metadata = tuf / "metadata"
entries = list(metadata.iterdir()) if metadata.exists() else []
if entries:
    print("initialized")
else:
    if tuf.exists() and {p.name for p in tuf.iterdir()} - {"metadata", "targets"}:
        raise SystemExit("uninitialized release has unexpected TUF entries")
    targets = tuf / "targets"
    if targets.exists() and any(targets.iterdir()):
        raise SystemExit("uninitialized release contains target blobs; restore the origin before publishing")
    print("fresh")
PY_STATE
)

# A successful prior activation deliberately retains its exchanged-out tree.
# It is safe to remove only now, before this release creates its transaction.
find "$staging" -mindepth 1 -maxdepth 1 -type d -name 'activation-*' -print0 | while IFS= read -r -d '' previous; do
  rm -rf -- "$previous"
done

transaction=$(mktemp -d "$staging/activation-${version}.XXXXXX")
next="$transaction/next"
mkdir "$next"
tar -xzf "$bundle" -C "$next" --no-same-owner --no-same-permissions

[[ -z "$(find "$next" -type l -print -quit)" ]] || { echo "staged release contains a symlink" >&2; exit 1; }
[[ "$(find "$next" -mindepth 1 -maxdepth 1 -printf '%f\n' | sort)" == 'tuf' ]] || { echo "staged release has an unexpected top-level file" >&2; exit 1; }

python3 - "$next/tuf/metadata" "$live/tuf/metadata" "$live_state" <<'PY_METADATA'
import json
import pathlib
import sys

def no_duplicates(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate metadata key")
        result[key] = value
    return result

def version(directory, role):
    path = directory / (role + ".json")
    if not path.is_file() or path.is_symlink():
        raise ValueError("metadata is unavailable")
    document = json.loads(path.read_text(), object_pairs_hook=no_duplicates)
    signed = document.get("signed")
    if not isinstance(signed, dict) or signed.get("_type") != role:
        raise ValueError("metadata role is invalid")
    value = signed.get("version")
    if type(value) is not int or value <= 0:
        raise ValueError("metadata version must be a positive integer")
    return value

candidate, live = map(pathlib.Path, sys.argv[1:3])
fresh = sys.argv[3] == "fresh"
try:
    candidate_targets = version(candidate, "targets")
    live_targets = version(live, "targets") if not fresh else None
    if not fresh and candidate_targets < live_targets:
        raise ValueError("metadata version rolls back live")
    if not fresh and candidate_targets == live_targets:
        # Snapshot/timestamp refresh owns no target, policy or root change.
        # Compare exact bytes under the same publication lock.
        for relative in ("tuf/metadata/root.json", "tuf/metadata/targets.json"):
            incoming = candidate.parent.parent / relative
            current = live.parent.parent / relative
            if not incoming.is_file() or incoming.is_symlink() or not current.is_file() or current.is_symlink() or incoming.read_bytes() != current.read_bytes():
                raise ValueError("unchanged targets refresh modifies root or targets")
except (OSError, ValueError, TypeError, AttributeError) as error:
    raise SystemExit(f"TUF targets: {error}")

for role in ("snapshot", "timestamp"):
    try:
        candidate_version = version(candidate, role)
        if not fresh and candidate_version <= version(live, role):
            raise ValueError("metadata version does not advance live")
    except (OSError, ValueError, TypeError, AttributeError) as error:
        raise SystemExit(f"TUF {role}: {error}")

PY_METADATA

python3 - "$next/tuf/metadata/targets.json" "$live/tuf/metadata/targets.json" "$version" "$live_state" <<'PY'
import datetime
import hashlib
import json
import pathlib
import re
import sys

targets_path = pathlib.Path(sys.argv[1])
live_targets_path = pathlib.Path(sys.argv[2])
version = sys.argv[3]
fresh = sys.argv[4] == "fresh"
expected = {
    "pb-darwin-arm64": ("darwin", "arm64", "macho"),
    "pb-linux-amd64": ("linux", "amd64", "elf"),
    "pb-linux-arm64": ("linux", "arm64", "elf"),
    "pb-windows-amd64.exe": ("windows", "amd64", "pe"),
    "pb-windows-arm64.exe": ("windows", "arm64", "pe"),
}

def reject_duplicate_keys(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError(f"duplicate JSON key: {key}")
        result[key] = value
    return result

def read_targets(path, label):
    if not path.is_file() or path.is_symlink():
        raise SystemExit(f"{label} TUF targets metadata is unavailable")
    try:
        document = json.loads(path.read_text(), object_pairs_hook=reject_duplicate_keys)
    except (OSError, json.JSONDecodeError, ValueError) as error:
        raise SystemExit(f"{label} TUF targets metadata is invalid: {error}")
    signed = document.get("signed") if isinstance(document, dict) else None
    if not isinstance(signed, dict):
        raise SystemExit(f"{label} TUF targets metadata has no signed object")
    targets = signed.get("targets")
    if not isinstance(targets, dict) or not targets:
        raise SystemExit(f"{label} TUF targets metadata does not contain the exact release asset set")
    # pb's targets plus, once published, the self-host bundle's (selfhost-release.yml), which pbh verifies.
    products = {name for name in targets if not name.startswith("selfhost/")}
    if label == "candidate" and products != set(expected):
        raise SystemExit(f"{label} TUF targets metadata does not contain the exact release asset set")
    # A product that joins the set (macOS after a Linux-only history) is not live yet; unknown ones never are.
    if label == "live" and not products <= set(expected):
        raise SystemExit("live TUF targets metadata contains an unknown release product")
    for name in set(targets) - products:
        validate_selfhost_target(name, targets[name])
    return targets

def validate_selfhost_target(name, target):
    match = re.fullmatch(r"selfhost/(20[0-9]{2}\.[0-9]{2}\.[0-9]{2}\.(?:0|[1-9][0-9]*))/(paperboat-selfhost-linux-(?:amd64|arm64)\.tar\.gz)", name)
    if not match or not isinstance(target, dict):
        raise SystemExit(f"self-host TUF target is invalid: {name}")
    length, digest, custom = target.get("length"), (target.get("hashes") or {}).get("sha256"), target.get("custom")
    if isinstance(length, bool) or not isinstance(length, int) or not 1 <= length <= 128 << 20 or not isinstance(digest, str) or not re.fullmatch(r"[0-9a-f]{64}", digest):
        raise SystemExit(f"self-host TUF target length or digest is invalid: {name}")
    repository = custom.get("repository") if isinstance(custom, dict) else None
    if not isinstance(repository, str) or not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repository) or set(custom) != {"repository", "url"}:
        raise SystemExit(f"self-host TUF target custom metadata is invalid: {name}")
    require_equal(custom["url"], f"https://github.com/{repository}/releases/download/selfhost-{match[1]}/{match[2]}", f"self-host TUF target download URL is invalid: {name}")
    return version_parts(match[1], name)

def version_parts(value, name):
    if not isinstance(value, str) or not re.fullmatch(r"20[0-9]{2}\.[0-9]{2}\.[0-9]{2}\.(0|[1-9][0-9]*)", value):
        raise SystemExit(f"TUF target version is invalid for {name}")
    year, month, day, release = value.split(".")
    try:
        datetime.date(int(year), int(month), int(day))
    except ValueError:
        raise SystemExit(f"TUF target version is invalid for {name}")
    return (year, month, day, release)

def compare_versions(left, right):
    for left_part, right_part in zip(left, right):
        if len(left_part) != len(right_part):
            return -1 if len(left_part) < len(right_part) else 1
        if left_part != right_part:
            return -1 if left_part < right_part else 1
    return 0

def validate_target(name, target):
    product = re.fullmatch(r"pb-(linux|darwin|windows)-(amd64|arm64)(\.exe)?", name)
    if not product:
        raise SystemExit(f"TUF target name is invalid: {name}")
    platform, architecture, suffix = product.groups()
    if bool(suffix) != (platform == "windows"):
        raise SystemExit(f"TUF target executable suffix is invalid: {name}")
    format_ = {"linux": "elf", "darwin": "macho", "windows": "pe"}[platform]
    if not isinstance(target, dict):
        raise SystemExit(f"TUF target metadata is invalid for {name}")
    hashes = target.get("hashes")
    length = target.get("length")
    if isinstance(length, bool) or not isinstance(length, int) or length < 1:
        raise SystemExit(f"TUF target length is invalid for {name}")
    if not isinstance(hashes, dict):
        raise SystemExit(f"TUF target hashes are invalid for {name}")
    digest = hashes.get("sha256")
    if not isinstance(digest, str) or not re.fullmatch(r"[0-9a-f]{64}", digest):
        raise SystemExit(f"TUF target digest is invalid for {name}")

    custom = target.get("custom")
    if not isinstance(custom, dict):
        raise SystemExit(f"TUF target custom metadata is invalid for {name}")
    target_version = custom.get("version")
    target_version_parts = version_parts(target_version, name)
    repository = custom.get("repository")
    if not isinstance(repository, str) or not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repository):
        raise SystemExit(f"TUF target repository is invalid for {name}")
    url = f"https://github.com/{repository}/releases/download/{target_version}/{name}"
    for key, wanted in {
        "schema": "paperboat.tuf-asset/v1",
        "kind": "github-release-asset",
        "version": target_version,
        "platform": platform,
        "architecture": architecture,
        "format": format_,
        "asset_name": name,
        "repository": repository,
        "url": url,
        "sha256": digest,
        "length": length,
    }.items():
        require_equal(custom.get(key), wanted, f"TUF custom metadata is invalid for {name}: {key}")

    release_index = custom.get("release_index")
    if not isinstance(release_index, dict):
        raise SystemExit(f"TUF release index is invalid for {name}")
    for key, wanted in {
        "schema": "paperboat.release-index/v1",
        "release_id": "rel_" + target_version,
        "version": target_version,
        "channel": "stable",
        "platform": platform,
        "architecture": architecture,
        "binary_format": format_,
    }.items():
        require_equal(release_index.get(key), wanted, f"TUF release index is invalid for {name}: {key}")
    severity = release_index.get("severity")
    if severity not in ("routine", "security", "critical"):
        raise SystemExit(f"TUF release index severity is invalid for {name}")
    policy_revision = release_index.get("rollout_policy_revision")
    if isinstance(policy_revision, bool) or not isinstance(policy_revision, int) or policy_revision < 1:
        raise SystemExit(f"TUF release index policy revision is invalid for {name}")

    index_targets = release_index.get("targets")
    if not isinstance(index_targets, list) or len(index_targets) != 1 or not isinstance(index_targets[0], dict):
        raise SystemExit(f"TUF release index target is invalid for {name}")
    index_target = index_targets[0]
    for key, wanted in {
        "component": "pb",
        "target_path": name,
        "asset_name": name,
        "repository": repository,
        "download_url": url,
        "sha256": digest,
        "length": length,
        "platform": platform,
        "architecture": architecture,
        "binary_format": format_,
    }.items():
        require_equal(index_target.get(key), wanted, f"TUF release index target is invalid for {name}: {key}")

    manifest_sha256 = release_index.get("manifest_sha256")
    if not isinstance(manifest_sha256, str) or not re.fullmatch(r"[0-9a-f]{64}", manifest_sha256):
        raise SystemExit(f"TUF release index manifest digest is invalid for {name}")
    deployment_plan_sha256 = release_index.get("deployment_plan_sha256")
    if not isinstance(deployment_plan_sha256, str) or not re.fullmatch(r"[0-9a-f]{64}", deployment_plan_sha256):
        raise SystemExit(f"TUF release index deployment-plan digest is invalid for {name}")
    deployment_plan = release_index.get("deployment_plan")
    if not isinstance(deployment_plan, dict):
        raise SystemExit(f"TUF release index deployment plan is missing for {name}")
    if deployment_plan.get("schema") != "paperboat.release-deployment/v1" or deployment_plan.get("version") != target_version or deployment_plan.get("manifest_sha256") != manifest_sha256:
        raise SystemExit(f"TUF release index deployment plan binding is invalid for {name}")
    expected_plan_fields = {
        "schema", "version", "manifest_sha256", "channel", "rollout_state", "severity",
        "policy_revision", "cohort_seed", "cohorts", "canary", "activation",
        "security_deferral", "rollback",
    }
    if set(deployment_plan) != expected_plan_fields:
        raise SystemExit(f"TUF release index deployment plan is incomplete for {name}")
    require_equal(deployment_plan.get("channel"), release_index.get("channel"), f"TUF release policy channel is invalid for {name}")
    require_equal(deployment_plan.get("severity"), severity, f"TUF release policy severity is invalid for {name}")
    require_equal(deployment_plan.get("policy_revision"), policy_revision, f"TUF release policy revision is invalid for {name}")
    plan_bytes = (json.dumps(deployment_plan, separators=(",", ":"), ensure_ascii=True) + "\n").encode()
    if hashlib.sha256(plan_bytes).hexdigest() != deployment_plan_sha256:
        raise SystemExit(f"TUF release index deployment-plan digest does not match for {name}")
    return target_version_parts, target_version, repository, url, digest, length

def require_equal(actual, wanted, message):
    if type(actual) is not type(wanted) or actual != wanted:
        raise SystemExit(message)

try:
    candidate_targets = read_targets(targets_path, "candidate")
    live_targets = read_targets(live_targets_path, "live") if not fresh else {}
except (OSError, ValueError) as error:
    raise SystemExit(f"TUF targets metadata is invalid: {error}")

refresh = not fresh and targets_path.read_bytes() == live_targets_path.read_bytes()
requested_version_parts = version_parts(version, "candidate")
candidate_repositories = set()
live_metadata = {name: validate_target(name, target) for name, target in live_targets.items() if not name.startswith("selfhost/")}
live_repositories = {value[2] for value in live_metadata.values()}
selected = []
for name in expected:
    candidate_version_parts, candidate_version, candidate_repository, url, digest, length = validate_target(name, candidate_targets[name])
    candidate_repositories.add(candidate_repository)
    if fresh:
        if candidate_version_parts != requested_version_parts:
            raise SystemExit("initial publication must select every launch target at the requested version")
        selected.append(name)
        continue
    if name not in live_metadata:
        if candidate_version_parts != requested_version_parts:
            raise SystemExit(f"a product new to the live set must be selected at the requested version: {name}")
        selected.append(name)
        continue
    live_version_parts, _, _, _, _, _ = live_metadata[name]
    if not refresh and candidate_targets[name] == live_targets[name]:
        continue  # kept as is: a self-host publication of the same release tag re-signs pb's targets unchanged
    if candidate_version_parts == requested_version_parts:
        if not refresh and compare_versions(candidate_version_parts, live_version_parts) <= 0:
            raise SystemExit(f"selected TUF target version does not advance the live target for {name}")
        selected.append(name)
    elif candidate_targets[name] != live_targets[name]:
        raise SystemExit(f"omitted TUF target metadata changed for {name}")

# The self-host bundle: kept byte-identical, or replaced by the requested version's, which must advance it.
candidate_selfhost = {name for name in candidate_targets if name.startswith("selfhost/")}
live_selfhost = {name for name in live_targets if name.startswith("selfhost/")}
added = {name for name in candidate_selfhost if candidate_targets[name] != live_targets.get(name)}
for name in added:
    parts = validate_selfhost_target(name, candidate_targets[name])
    if parts != requested_version_parts or any(compare_versions(parts, validate_selfhost_target(old, live_targets[old])) <= 0 for old in live_selfhost):
        raise SystemExit(f"self-host TUF target is not the requested version or does not advance the live bundle: {name}")
    selected.append(name)
if (live_selfhost - candidate_selfhost) and not added:
    raise SystemExit("candidate TUF targets drop the live self-host bundle")

if refresh and len(selected) != len(expected):
    raise SystemExit("refreshed TUF targets must all retain the requested release version")
if len(selected) == 0:
    raise SystemExit("TUF targets metadata does not select any target at the requested release version")
if len(candidate_repositories) != 1 or (not fresh and (len(live_repositories) != 1 or live_repositories != candidate_repositories)):
    raise SystemExit("TUF targets disagree on the immutable GitHub repository")

PY

for required in tuf/metadata/root.json tuf/metadata/targets.json tuf/metadata/snapshot.json tuf/metadata/timestamp.json; do
  [[ -s "$next/$required" && ! -L "$next/$required" ]] || { echo "release bundle is missing $required" >&2; exit 1; }
done
for directory in "$next/tuf/metadata" "$next/tuf/targets"; do
  [[ -d "$directory" && ! -L "$directory" ]] || { echo "release bundle is missing a TUF directory" >&2; exit 1; }
  [[ -z "$(find "$directory" -mindepth 1 -type d -print -quit)" ]] || { echo "release bundle contains nested TUF paths" >&2; exit 1; }
  [[ -z "$(find "$directory" -mindepth 1 ! -type f -print -quit)" ]] || { echo "release bundle contains a non-regular TUF file" >&2; exit 1; }
done
[[ -z "$(find "$next/tuf/targets" -mindepth 1 -print -quit)" ]] || { echo "release bundle must not contain TUF target blobs" >&2; exit 1; }

chown -R 501:root "$next"
chmod 0700 "$next"
verify_live_mount_contract "$release_root"

# This must remain the final command. The server resolves current through the
# releases-parent mount on every request, so the exchange exposes the TUF
# metadata at once. The old tree stays in transaction/next
# until a later release performs its pre-activation cleanup.
atomic_exchange "$live" "$next"
