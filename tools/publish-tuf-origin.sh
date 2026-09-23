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
  [[ -d "$live/tuf/metadata" && ! -L "$live/tuf/metadata" ]] || { echo "live TUF metadata is unavailable" >&2; return 1; }
  [[ -d "$live/tuf/targets" && ! -L "$live/tuf/targets" ]] || { echo "live TUF targets are unavailable" >&2; return 1; }
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
[[ "$(find "$next" -mindepth 1 -maxdepth 1 -printf '%f\n' | sort)" == $'install\ntuf\nwindows' ]] || { echo "staged release has an unexpected top-level file" >&2; exit 1; }

python3 - "$next/tuf/metadata/targets.json" "$version" <<'PY'
import hashlib
import json, pathlib, re, sys
targets_path = pathlib.Path(sys.argv[1])
version = sys.argv[2]
expected = {
    "pb-darwin-arm64.pkg": ("darwin", "arm64", "pkg"),
    "pb-linux-amd64": ("linux", "amd64", "elf"),
    "pb-linux-arm64": ("linux", "arm64", "elf"),
    "pb-windows-amd64.exe": ("windows", "amd64", "pe"),
    "pb-windows-arm64.exe": ("windows", "arm64", "pe"),
}
try:
    targets_document = json.loads(targets_path.read_text())
except (OSError, json.JSONDecodeError) as error:
    raise SystemExit(f"TUF targets metadata is invalid: {error}")
signed = targets_document.get("signed")
if not isinstance(signed, dict):
    raise SystemExit("TUF targets metadata has no signed object")
targets = signed.get("targets")
if not isinstance(targets, dict) or set(targets) != set(expected):
    raise SystemExit("TUF targets metadata does not contain the exact release asset set")

def require_equal(actual, wanted, message):
    if type(actual) is not type(wanted) or actual != wanted:
        raise SystemExit(message)

release_repository = None
for name, (platform, architecture, format_) in expected.items():
    target = targets.get(name)
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
    repository = custom.get("repository")
    if not isinstance(repository, str) or not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repository):
        raise SystemExit(f"TUF target repository is invalid for {name}")
    if release_repository is None:
        release_repository = repository
    require_equal(repository, release_repository, f"TUF targets disagree on repository for {name}")
    url = f"https://github.com/{repository}/releases/download/{version}/{name}"
    for key, wanted in {
        "schema": "paperboat.tuf-asset/v1",
        "kind": "github-release-asset",
        "version": version,
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
    require_equal(release_index.get("schema"), "paperboat.release-index/v1", f"TUF release index schema is invalid for {name}")
    require_equal(release_index.get("release_id"), "rel_" + version, f"TUF release index ID is invalid for {name}")
    require_equal(release_index.get("version"), version, f"TUF release index version is invalid for {name}")
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

    # These fields are mandatory in the current release-index contract. A
    # missing or null value is not a usable policy and must never cross the
    # final pre-exchange boundary.
    manifest_sha256 = release_index.get("manifest_sha256")
    if not isinstance(manifest_sha256, str) or not re.fullmatch(r"[0-9a-f]{64}", manifest_sha256):
        raise SystemExit(f"TUF release index manifest digest is invalid for {name}")
    deployment_plan_sha256 = release_index.get("deployment_plan_sha256")
    if not isinstance(deployment_plan_sha256, str) or not re.fullmatch(r"[0-9a-f]{64}", deployment_plan_sha256):
        raise SystemExit(f"TUF release index deployment-plan digest is invalid for {name}")
    deployment_plan = release_index.get("deployment_plan")
    if not isinstance(deployment_plan, dict):
        raise SystemExit(f"TUF release index deployment plan is missing for {name}")
    if deployment_plan.get("schema") != "paperboat.release-deployment/v1" or deployment_plan.get("version") != version or deployment_plan.get("manifest_sha256") != manifest_sha256:
        raise SystemExit(f"TUF release index deployment plan binding is invalid for {name}")
    expected_plan_fields = {
        "schema", "version", "manifest_sha256", "channel", "rollout_state", "severity",
        "policy_revision", "cohort_seed", "cohorts", "canary", "activation",
        "security_deferral", "rollback",
    }
    if set(deployment_plan) != expected_plan_fields:
        raise SystemExit(f"TUF release index deployment plan is incomplete for {name}")
    plan_bytes = (json.dumps(deployment_plan, separators=(",", ":"), ensure_ascii=True) + "\n").encode()
    if hashlib.sha256(plan_bytes).hexdigest() != deployment_plan_sha256:
        raise SystemExit(f"TUF release index deployment-plan digest does not match for {name}")

root = targets_path.parent.parent.parent
for script, version_pin, repository_pin in (
    ("install", f"bootstrap_version='{version}'", f"repository=${{PAPERBOAT_GITHUB_REPOSITORY:-{release_repository}}}"),
    ("windows", f"$bootstrapVersion = '{version}'", f"else {{ '{release_repository}' }}"),
):
    body = (root / script).read_text(encoding="utf-8")
    if version_pin not in body or repository_pin not in body or "@PAPERBOAT_BOOTSTRAP_" in body:
        raise SystemExit(f"{script} verifier pins do not match the signed release")
PY

for required in install windows tuf/metadata/root.json tuf/metadata/targets.json tuf/metadata/snapshot.json tuf/metadata/timestamp.json; do
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
# releases-parent mount on every request, so the exchange exposes TUF,
# installers together. The old tree stays in transaction/next
# until a later release performs its pre-activation cleanup.
atomic_exchange "$live" "$next"
