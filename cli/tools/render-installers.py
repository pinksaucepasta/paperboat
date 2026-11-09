#!/usr/bin/env python3
"""Render HTTPS installers with pins from the verified TUF publication records."""

import json
import os
import pathlib
import re
import sys


def main() -> None:
    if len(sys.argv) != 4:
        raise SystemExit("usage: render-installers.py VERIFIED_TARGETS_JSON OUTPUT_INSTALL OUTPUT_WINDOWS")
    metadata, output_install, output_windows = sys.argv[1:]
    document = json.loads(pathlib.Path(metadata).read_text(encoding="utf-8"))
    targets = document["signed"]["targets"]
    assets = {
        "pb-linux-amd64": ("linux", "amd64", "elf"),
        "pb-linux-arm64": ("linux", "arm64", "elf"),
        "pb-darwin-arm64": ("darwin", "arm64", "macho"),
        "pb-windows-amd64.exe": ("windows", "amd64", "pe"),
        "pb-windows-arm64.exe": ("windows", "arm64", "pe"),
    }
    if set(targets) != set(assets):
        raise SystemExit("verified publication must contain exactly five product targets")
    values = {}
    repositories = set()
    for name, (platform, architecture, binary_format) in assets.items():
        target = targets[name]
        custom = target["custom"]
        version, repository, digest, length = (custom[key] for key in ("version", "repository", "sha256", "length"))
        if not isinstance(version, str) or not re.fullmatch(r"20[0-9]{2}\.[0-9]{2}\.[0-9]{2}\.(0|[1-9][0-9]*)", version):
            raise SystemExit(f"invalid product version: {name}")
        if not isinstance(repository, str) or not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repository):
            raise SystemExit(f"invalid product repository: {name}")
        repositories.add(repository)
        url = f"https://github.com/{repository}/releases/download/{version}/{name}"
        expected = {"schema": "paperboat.tuf-asset/v1", "kind": "github-release-asset", "version": version, "platform": platform, "architecture": architecture, "format": binary_format, "asset_name": name, "repository": repository, "url": url, "sha256": digest, "length": length}
        if any(custom.get(key) != value for key, value in expected.items()):
            raise SystemExit(f"invalid product coordinates: {name}")
        if not isinstance(digest, str) or not re.fullmatch(r"[a-f0-9]{64}", digest) or isinstance(length, bool) or not isinstance(length, int) or not 0 < length <= 512 << 20:
            raise SystemExit(f"invalid product byte identity: {name}")
        if target.get("length") != length or target.get("hashes", {}).get("sha256") != digest:
            raise SystemExit(f"inconsistent verified product identity: {name}")
        prefix = f"PAPERBOAT_PRODUCT_{platform}_{architecture}".upper()
        values.update({f"{prefix}_VERSION": version, f"{prefix}_URL": url, f"{prefix}_SHA256": digest, f"{prefix}_LENGTH": str(length)})
        if platform == "windows":
            # The signed release index says whether the binary carries the publisher's Authenticode signature.
            marked = custom.get("release_index", {}).get("targets", [{}])[0].get("authenticode_signed", False)
            if not isinstance(marked, bool):
                raise SystemExit(f"invalid Authenticode mark: {name}")
            values[f"{prefix}_AUTHENTICODE"] = "true" if marked else "false"
    if len(repositories) != 1:
        raise SystemExit("verified product targets must share one immutable repository")
    values["PAPERBOAT_PRODUCT_REPOSITORY"] = repositories.pop()
    # The expected signer's certificate simple name; the release step that marks targets signed supplies it.
    publisher = os.environ.get("PAPERBOAT_WINDOWS_PUBLISHER", "")
    marked_any = "true" in (values["PAPERBOAT_PRODUCT_WINDOWS_AMD64_AUTHENTICODE"], values["PAPERBOAT_PRODUCT_WINDOWS_ARM64_AUTHENTICODE"])
    if marked_any and not re.fullmatch(r"[A-Za-z0-9 .,&()-]{1,64}", publisher):
        raise SystemExit("signed Windows targets need PAPERBOAT_WINDOWS_PUBLISHER, the exact signer certificate name")
    values["PAPERBOAT_WINDOWS_PUBLISHER"] = publisher
    root = pathlib.Path(__file__).resolve().parent
    for template, output in ((root / "install.sh", pathlib.Path(output_install)), (root / "install.ps1", pathlib.Path(output_windows))):
        body = template.read_text(encoding="utf-8")
        for key, value in values.items():
            body = body.replace("@" + key + "@", value)
        if re.search(r"@PAPERBOAT_[A-Z0-9_]+@", body):
            raise SystemExit(f"unrendered product pin in {template}")
        output.write_text(body, encoding="utf-8")


if __name__ == "__main__":
    main()
