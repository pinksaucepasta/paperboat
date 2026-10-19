#!/usr/bin/env python3
"""Fails when documentation names a PAPERBOAT_* environment variable no code or deployment file mentions.

Usage: tools/check_doc_envvars.py          (run by tools/gate.sh)

A documented `<NAME>_FILE` is accepted when NAME exists: the server reads every secret
setting from a file named that way. Planning files (PLAN, CHECKLIST, WHITEBOARD) are skipped.
"""
import re
import subprocess
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
VARIABLE = re.compile(r"\bPAPERBOAT_[A-Z0-9]+(?:_[A-Z0-9]+)*\b")
PLANNING = {"PLAN.md", "CHECKLIST.md", "WHITEBOARD.md", "AGENTS.md", "REMOTE_TESTING.md"}
CODE_SUFFIXES = (".go", ".py", ".sh", ".yml", ".yaml", ".service", ".timer", ".json", ".ts", ".tsx", ".js", ".mjs", ".ps1",
                 ".env", ".toml", ".sql", "Dockerfile", ".Dockerfile")


def undocumented(docs: dict[str, str], code: str) -> list[str]:
    found = []
    for path in sorted(docs):
        for name in sorted(set(VARIABLE.findall(docs[path]))):
            base = name[:-5] if name.endswith("_FILE") else name
            if name in code or base in code:
                continue
            found.append(f"{path}: {name}")
    return found


def tracked() -> tuple[dict[str, str], str]:
    names = subprocess.run(["git", "-C", str(REPO), "ls-files", "-z", "--cached", "--others", "--exclude-standard"],
                           capture_output=True, check=True).stdout.decode().split("\0")
    docs, code = {}, []
    for name in filter(None, names):
        path = REPO / name
        if not path.is_file() or "node_modules/" in name or "/testdata/" in name:
            continue
        if name.endswith(".md"):
            if Path(name).name not in PLANNING:
                docs[name] = path.read_text("utf-8", "replace")
        elif name.endswith(CODE_SUFFIXES) or "/" not in name or Path(name).name.startswith("Dockerfile"):
            code.append(path.read_text("utf-8", "replace"))
    return docs, "\n".join(code)


def main() -> int:
    docs, code = tracked()
    problems = undocumented(docs, code)
    if problems:
        print("Documentation names environment variables that nothing reads or sets:")
        print("\n".join("  " + p for p in problems))
        return 1
    print(f"OK: every PAPERBOAT_* variable in {len(docs)} Markdown files exists in code or deployment files")
    return 0


if __name__ == "__main__":
    sys.exit(main())
