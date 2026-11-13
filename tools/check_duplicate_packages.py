#!/usr/bin/env python3
"""Fail if two Go package directories in different modules are byte-identical (F21-33)."""
import hashlib, os, sys

MODULES = ["cli", "server", "relay", "helper", "edge", "shared"]


def packages(root):
    for module in MODULES:
        for base, dirs, files in os.walk(os.path.join(root, module)):
            dirs[:] = [d for d in dirs if d not in ("node_modules", "testdata", ".git")]
            go = sorted(f for f in files if f.endswith(".go"))
            if go:
                yield base, module, tuple(
                    (f, hashlib.sha256(open(os.path.join(base, f), "rb").read()).hexdigest()) for f in go
                )


def duplicates(root):
    seen, found = {}, []
    for base, module, key in packages(root):
        other = seen.setdefault(key, (base, module))
        if other[0] != base and other[1] != module:
            found.append((os.path.relpath(other[0], root), os.path.relpath(base, root)))
    return found


if __name__ == "__main__":
    dups = duplicates(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
    for a, b in dups:
        print(f"byte-identical packages: {a} == {b}; keep one in shared/")
    sys.exit(1 if dups else 0)
