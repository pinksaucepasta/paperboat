#!/usr/bin/env python3
"""Render release installers with pins for their first-install TUF verifier."""

import hashlib
import pathlib
import re
import sys


def main() -> None:
    if len(sys.argv) != 6:
        raise SystemExit("usage: render-installers.py VERSION REPOSITORY VERIFIER_DIR OUTPUT_INSTALL OUTPUT_WINDOWS")
    version, repository, source, output_install, output_windows = sys.argv[1:]
    if not re.fullmatch(r"20[0-9]{2}\.[0-9]{2}\.[0-9]{2}\.(0|[1-9][0-9]*)", version):
        raise SystemExit("invalid release version")
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repository):
        raise SystemExit("invalid GitHub repository")
    root = pathlib.Path(__file__).resolve().parent
    source = pathlib.Path(source)
    values = {"PAPERBOAT_BOOTSTRAP_VERSION": version, "PAPERBOAT_BOOTSTRAP_REPOSITORY": repository}
    for platform, architecture in (("linux", "amd64"), ("linux", "arm64"), ("darwin", "arm64"), ("windows", "amd64"), ("windows", "arm64")):
        name = f"pb-bootstrap-{platform}-{architecture}" + (".exe" if platform == "windows" else "")
        path = source / name
        if not path.is_file() or path.is_symlink() or path.stat().st_size < 1:
            raise SystemExit(f"invalid bootstrap verifier: {name}")
        key = f"PAPERBOAT_BOOTSTRAP_{platform}_{architecture}".upper()
        with path.open("rb") as verifier:
            values[f"{key}_SHA256"] = hashlib.file_digest(verifier, "sha256").hexdigest()
        values[f"{key}_LENGTH"] = str(path.stat().st_size)
    for template, output in ((root / "install.sh", pathlib.Path(output_install)), (root / "install.ps1", pathlib.Path(output_windows))):
        body = template.read_text(encoding="utf-8")
        for key, value in values.items():
            body = body.replace("@" + key + "@", value)
        if re.search(r"@PAPERBOAT_[A-Z0-9_]+@", body):
            raise SystemExit(f"unrendered bootstrap pin in {template}")
        output.write_text(body, encoding="utf-8")


if __name__ == "__main__":
    main()
