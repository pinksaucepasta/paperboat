#!/usr/bin/env python3
"""Verify reviewed, version-pinned licenses before excluding them from classification."""

import hashlib
import json
import pathlib
import re
import sys


def module_records(text):
    decoder = json.JSONDecoder()
    records = {}
    position = 0
    while position < len(text):
        while position < len(text) and text[position].isspace():
            position += 1
        if position == len(text):
            break
        record, position = decoder.raw_decode(text, position)
        records[record["Path"]] = record
    return records


def license_digest(root, relative):
    root = pathlib.Path(root).resolve()
    path = (root / relative).resolve()
    if not path.is_relative_to(root) or path == root:
        raise ValueError("reviewed license path must stay inside its recorded directory")
    with path.open("rb") as source:
        return hashlib.file_digest(source, "sha256").hexdigest()


def reviewed_ignores(registry, modules, recorded_root):
    versions = {}
    for identity, review in registry.items():
        module, separator, version = identity.rpartition("@")
        if not separator or not module or not version.startswith("v"):
            raise ValueError("license exception must name module@version")
        versions.setdefault(module, {})[version] = review
    ignored = []
    for module, reviews in versions.items():
        selected = modules.get(module)
        if selected is None:
            continue  # A recorded review never excludes a module absent from this graph.
        version = selected.get("Version", "")
        if version not in reviews or selected.get("Replace"):
            raise ValueError(f"{module}@{version}: version or replacement requires license review")
        if any(path != module and path.startswith(module) for path in modules):
            raise ValueError(f"{module}: ignore prefix would cover another module")
        review = reviews[version]
        if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9.+-]*", review["spdx"]):
            raise ValueError(f"{module}: reviewed SPDX identifier is invalid")
        expected = review["sha256"]
        if not re.fullmatch(r"[0-9a-f]{64}", expected):
            raise ValueError(f"{module}: reviewed SHA256 is invalid")
        if license_digest(recorded_root, review["license_file"]) != expected:
            raise ValueError(f"{module}@{version}: recorded license differs from the reviewed SHA256")
        if not selected.get("Dir"):
            raise ValueError(f"{module}@{version}: download the selected module before license verification")
        if license_digest(selected["Dir"], review["module_license_file"]) != expected:
            raise ValueError(f"{module}@{version}: module license differs from its reviewed copy")
        print(f"verified {module}@{version}: {review['spdx']}", file=sys.stderr)
        ignored.append(module)
    return ignored


def main():
    if len(sys.argv) != 3:
        raise ValueError("usage: verify-license-exceptions.py registry.json selected-modules.json")
    registry_path = pathlib.Path(sys.argv[1])
    registry = json.loads(registry_path.read_text())
    modules = module_records(pathlib.Path(sys.argv[2]).read_text())
    ignored = reviewed_ignores(registry, modules, registry_path.parent)
    for module in ignored:
        print(module)


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, KeyError, TypeError) as error:
        sys.exit(f"license exception verification failed: {error}")
