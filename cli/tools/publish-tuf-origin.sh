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

python3 - "$next/tuf/metadata/targets.json" "$live/tuf/metadata/targets.json" "$version" <<'PY'
import datetime
import hashlib
import json
import pathlib
import re
import sys

targets_path = pathlib.Path(sys.argv[1])
live_targets_path = pathlib.Path(sys.argv[2])
version = sys.argv[3]
expected = {
    "pb-darwin-arm64.pkg": ("darwin", "arm64", "pkg"),
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
    if not isinstance(targets, dict) or set(targets) != set(expected):
        raise SystemExit(f"{label} TUF targets metadata does not contain the exact release asset set")
    return targets

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
    platform, architecture, format_ = expected[name]
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
    live_targets = read_targets(live_targets_path, "live")
except (OSError, ValueError) as error:
    raise SystemExit(f"TUF targets metadata is invalid: {error}")

requested_version_parts = version_parts(version, "candidate")
candidate_repositories = set()
live_repositories = set()
selected = []
candidate_product_pins = {}
for name in expected:
    live_version_parts, _, live_repository, _, _, _ = validate_target(name, live_targets[name])
    candidate_version_parts, candidate_version, candidate_repository, url, digest, length = validate_target(name, candidate_targets[name])
    candidate_product_pins[name] = (candidate_version, url, digest, length)
    live_repositories.add(live_repository)
    candidate_repositories.add(candidate_repository)
    if candidate_version_parts == requested_version_parts:
        if compare_versions(candidate_version_parts, live_version_parts) <= 0:
            raise SystemExit(f"selected TUF target version does not advance the live target for {name}")
        selected.append(name)
    elif candidate_targets[name] != live_targets[name]:
        raise SystemExit(f"omitted TUF target metadata changed for {name}")

if len(selected) == 0:
    raise SystemExit("TUF targets metadata does not select any target at the requested release version")
if len(live_repositories) != 1 or len(candidate_repositories) != 1 or live_repositories != candidate_repositories:
    raise SystemExit("TUF targets disagree on the immutable GitHub repository")

root = targets_path.parent.parent.parent
def read_installer(name):
    try:
        body = (root / name).read_text(encoding="utf-8")
    except (OSError, UnicodeError) as error:
        raise SystemExit(f"{name} installer is unavailable or invalid: {error}")
    old_markers = ("pb-bootstrap", "paperboat_bootstrap_", "bootstrap_version", "$bootstrapversion", "paperboat_github_repository", "verifier")
    if re.search(r"@PAPERBOAT_[A-Z0-9_]+@", body) or any(marker in body.lower() for marker in old_markers):
        raise SystemExit(f"{name} installer contains an unresolved pin or removed verifier reference")
    return body

def shell_product_branches(body):
    branch_names = ("linux-amd64", "linux-arm64", "darwin-arm64")
    matches = list(re.finditer(r"(?m)^[ \t]*(linux-amd64|linux-arm64|darwin-arm64)\)[ \t]*$", body))
    if tuple(match.group(1) for match in matches) != branch_names:
        raise SystemExit("shell installer product branches are missing, duplicated, or reordered")
    branches = {}
    for index, match in enumerate(matches):
        if index + 1 < len(matches):
            end = matches[index + 1].start()
        else:
            default_branch = re.search(r"(?m)^[ \t]*\*\)", body[match.end():])
            if default_branch is None:
                raise SystemExit("shell installer default platform branch is missing")
            end = match.end() + default_branch.start()
        branches[match.group(1)] = body[match.start():end]
    return branches

def windows_product_branches(body):
    start_matches = list(re.finditer(r"(?m)^if \(\$arch -eq 'amd64'\) \{[ \t]*$", body))
    if len(start_matches) != 1:
        raise SystemExit("Windows installer architecture branches are missing or ambiguous")
    else_match = re.search(r"(?m)^\} else \{[ \t]*$", body[start_matches[0].end():])
    if else_match is None:
        raise SystemExit("Windows installer ARM64 branch is missing")
    else_start = start_matches[0].end() + else_match.start()
    else_end = start_matches[0].end() + else_match.end()
    end_match = re.search(r"(?m)^\}[ \t]*$", body[else_end:])
    if end_match is None:
        raise SystemExit("Windows installer ARM64 branch is unterminated")
    arm_end = else_end + end_match.start()
    return {
        "amd64": body[start_matches[0].end():else_start],
        "arm64": body[else_end:arm_end],
    }

def require_product_pins(branch, name, pin, powershell):
    version, url, digest, length = pin
    fields = (
        ("version", "$productVersion" if powershell else "product_version", version),
        ("URL", "$productUrl" if powershell else "product_url", url),
        ("SHA-256", "$productSha" if powershell else "product_sha", digest),
        ("length", "$productLength" if powershell else "product_length", str(length)),
    )
    for field, variable, value in fields:
        variable_pattern = re.escape(variable)
        if powershell:
            pattern = re.compile(rf"(?m)^[ \t]*{variable_pattern}[ \t]*=[ \t]*'([^']*)'[ \t]*$")
        else:
            pattern = re.compile(rf"(?m)^[ \t]*{variable_pattern}='([^']*)'[ \t]*(?:;;)?[ \t]*$")
        matches = pattern.findall(branch)
        if len(matches) != 1 or matches[0] != value:
            raise SystemExit(f"installer product pin mismatch for {name}: {field}")

install_body = read_installer("install")
install_branches = shell_product_branches(install_body)
for name, branch_name in (
    ("pb-linux-amd64", "linux-amd64"),
    ("pb-linux-arm64", "linux-arm64"),
    ("pb-darwin-arm64.pkg", "darwin-arm64"),
):
    require_product_pins(install_branches[branch_name], name, candidate_product_pins[name], False)

windows_body = read_installer("windows")
windows_branches = windows_product_branches(windows_body)
for name, architecture in (
    ("pb-windows-amd64.exe", "amd64"),
    ("pb-windows-arm64.exe", "arm64"),
):
    require_product_pins(windows_branches[architecture], name, candidate_product_pins[name], True)
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

# Selfhost has a separate release owner. Carry its existing public distribution
# through this CLI metadata/installer transaction instead of deleting its routes.
python3 - "$live/selfhost" "$next/selfhost" <<'PY'
import os
import pathlib
import re
import stat
import sys

source, destination = map(pathlib.Path, sys.argv[1:])
if not source.exists() and not source.is_symlink():
    raise SystemExit(0)
files = []
total = 0

def reject(message):
    raise SystemExit("live selfhost distribution is unsafe: " + message)

def directory(path):
    if not stat.S_ISDIR(path.lstat().st_mode):
        reject("non-directory or symlink")

def regular(path, limit):
    global total
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or not 0 < info.st_size <= limit:
        reject("non-regular, empty, or oversized file")
    total += info.st_size
    if total > 2 << 30:
        reject("distribution exceeds 2 GiB preservation bound")
    files.append((path, info))

directory(source)
entries = {p.name for p in source.iterdir()}
if not {"install", "manifest.json"} <= entries or entries - {"install", "manifest.json", "versions"}:
    reject("unexpected top-level entry")
regular(source / "install", 1 << 20)
regular(source / "manifest.json", 64 << 10)
versions = source / "versions"
releases = []
if "versions" in entries:
    directory(versions)
    releases = list(versions.iterdir())
if len(releases) > 32:
    reject("version count exceeds preservation bound")
for release in releases:
    if re.fullmatch(r"[0-9.]{1,128}", release.name) is None:
        reject("unexpected version path")
    directory(release)
    entries = list(release.iterdir())
    packages = 0
    for entry in entries:
        if entry.name in {"paperboat-selfhost-linux-amd64.tar.gz", "paperboat-selfhost-linux-arm64.tar.gz"}:
            regular(entry, 512 << 20)
            packages += 1
        elif entry.name == "install":
            regular(entry, 1 << 20)
        elif entry.name == "manifest.json":
            regular(entry, 64 << 10)
        else:
            reject("unexpected version entry")
    if packages == 0:
        reject("version has no package")

# Validate before copying; O_NOFOLLOW and a stable open-file identity also
# prevent a file replacement from being followed during the copy.
for path, expected in files:
    target = destination / path.relative_to(source)
    target.parent.mkdir(parents=True, exist_ok=True)
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(fd, "rb") as input_file:
        actual = os.fstat(input_file.fileno())
        if not stat.S_ISREG(actual.st_mode) or (actual.st_dev, actual.st_ino, actual.st_size, actual.st_mtime_ns) != (expected.st_dev, expected.st_ino, expected.st_size, expected.st_mtime_ns):
            reject("file changed before preservation")
        with target.open("xb") as output_file:
            remaining = actual.st_size
            while remaining:
                block = input_file.read(min(remaining, 1 << 20))
                if not block:
                    reject("file truncated during preservation")
                output_file.write(block)
                remaining -= len(block)
            if input_file.read(1):
                reject("file grew during preservation")
        after = os.fstat(input_file.fileno())
        if (after.st_size, after.st_mtime_ns) != (actual.st_size, actual.st_mtime_ns) or target.stat().st_size != actual.st_size:
            reject("file changed during preservation")
    target.chmod(stat.S_IMODE(expected.st_mode) & 0o755)
PY

chown -R 501:root "$next"
chmod 0700 "$next"
verify_live_mount_contract "$release_root"

# This must remain the final command. The server resolves current through the
# releases-parent mount on every request, so the exchange exposes TUF,
# installers and the preserved selfhost distribution together. The old tree stays in transaction/next
# until a later release performs its pre-activation cleanup.
atomic_exchange "$live" "$next"
