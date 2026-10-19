#!/usr/bin/env python3
"""Fails when a production domain is hardcoded in code (AGENTS.md "Code quality").

Usage: tools/check_domains.py          (run by tools/gate.sh)

Domains come from configuration, the server's discovery response, or the build's
API address; code and tests use the configured test domain (example.test). Prose
documentation (*.md, generated reference pages) may show the real hostnames users
type, so it is not scanned. Everything else tracked by git is.
"""
import re
import subprocess
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent

DOMAIN = re.compile(r"pprbt\.dev|pprbt\.app|ppr\.boats|paperboat\.cloud|newt-mermaid\.ts\.net", re.I)

# Documentation and the planning files name real hostnames on purpose.
DOC_SUFFIXES = (".md", ".1")
DOC_DIRS = ("/docs/",)

# Build configuration is where the initial API address belongs (AGENTS.md).
ALLOWED = {
    "cli/Makefile": "DEFAULT_SERVER_URL: the release build's initial API address",
    "tools/check_domains.py": "this check",
    "tools/test_check_domains.py": "this check's tests",
}


def scanned(path: str) -> bool:
    if path in ALLOWED or path.endswith(DOC_SUFFIXES):
        return False
    return not any(part in "/" + path for part in DOC_DIRS)


def violations(files: dict[str, str]) -> list[str]:
    found = []
    for path in sorted(files):
        if not scanned(path):
            continue
        for number, line in enumerate(files[path].splitlines(), 1):
            match = DOMAIN.search(line)
            if match:
                found.append(f"{path}:{number}: {match.group(0)}")
    return found


def tracked_files() -> dict[str, str]:
    names = subprocess.run(
        ["git", "-C", str(REPO), "ls-files", "-z", "--cached", "--others", "--exclude-standard"],
        capture_output=True, check=True).stdout.decode().split("\0")
    files = {}
    for name in filter(None, names):
        path = REPO / name
        if not path.is_file() or not scanned(name):
            continue
        data = path.read_bytes()
        if b"\0" not in data[:4096]:
            files[name] = data.decode("utf-8", "replace")
    return files


def main() -> int:
    found = violations(tracked_files())
    if found:
        print("Hardcoded domains (use configuration, discovery or example.test):")
        print("\n".join("  " + line for line in found))
        return 1
    print("OK: no hardcoded domains")
    return 0


if __name__ == "__main__":
    sys.exit(main())
