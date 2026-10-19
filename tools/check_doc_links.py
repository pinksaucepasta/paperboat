#!/usr/bin/env python3
"""Fails when a relative link in a tracked Markdown file points at a missing file.

Usage: tools/check_doc_links.py          (run by tools/gate.sh)

Links to other sites, to anchors within the same page, and mailto: are ignored.
A link to a file may carry an #anchor; only the file must exist.
"""
import re
import subprocess
import sys
from pathlib import Path
from urllib.parse import unquote

REPO = Path(__file__).resolve().parent.parent
LINK = re.compile(r"(?<!\!)\[[^\]\n]*\]\(([^)\s]+)(?:\s+\"[^\"]*\")?\)|(?<=\!)\[[^\]\n]*\]\(([^)\s]+)\)")
FENCE = re.compile(r"^\s*(```|~~~)")


def targets(text: str):
    """Yield (line number, link target) outside fenced code blocks."""
    fenced = False
    for number, line in enumerate(text.splitlines(), 1):
        if FENCE.match(line):
            fenced = not fenced
            continue
        if fenced:
            continue
        line = re.sub(r"`[^`\n]*`", "", line)
        for match in LINK.finditer(line):
            yield number, match.group(1) or match.group(2)


def broken(files: dict[str, str], exists) -> list[str]:
    found = []
    for path in sorted(files):
        for number, target in targets(files[path]):
            if re.match(r"^([a-z][a-z0-9+.-]*:|#|//)", target, re.I):
                continue
            relative = unquote(target.split("#", 1)[0])
            if not relative:
                continue
            resolved = (Path(path).parent / relative) if not relative.startswith("/") else Path(relative.lstrip("/"))
            if not exists(Path(resolved.as_posix())):
                found.append(f"{path}:{number}: {target}")
    return found


def main() -> int:
    names = subprocess.run(["git", "-C", str(REPO), "ls-files", "-z", "--cached", "--others", "--exclude-standard", "*.md"],
                           capture_output=True, check=True).stdout.decode().split("\0")
    files = {n: (REPO / n).read_text("utf-8", "replace") for n in names if n and (REPO / n).is_file()
             and "node_modules/" not in n and "/testdata/" not in n}
    problems = broken(files, lambda p: (REPO / p).exists() or (REPO / p).is_symlink())
    if problems:
        print("Broken relative links in documentation:")
        print("\n".join("  " + p for p in problems))
        return 1
    print(f"OK: relative links resolve in {len(files)} Markdown files")
    return 0


if __name__ == "__main__":
    sys.exit(main())
