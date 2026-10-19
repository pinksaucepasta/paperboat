#!/usr/bin/env python3
"""Server first, then client: the deployed server must contain every server/ and shared/ commit
of the commit being released or pushed.

Usage: tools/check_server_deployed.py --server-url URL [--release REV] [--warn]

Reads the running server's source commit from GET /v1/server-version and lists the commits of
server/ and shared/ in DEPLOYED..REV (default HEAD). Any such commit is not live yet, so a client
built from REV could talk to a server that does not understand it. Exit 1 when there are some
(or the server cannot be read, which fails closed); with --warn the same findings only print
and the exit status is 0 (the `make gate` warning step).
"""
import argparse
import json
import re
import subprocess
import sys
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
PATHS = ("server", "shared")
COMMIT = re.compile(r"^[0-9a-f]{40}$")


class Refusal(Exception):
    pass


def deployed_commit(server_url: str, timeout: float = 15.0) -> str:
    url = urllib.parse.urlsplit(server_url)
    loopback = url.hostname in ("127.0.0.1", "localhost", "::1")
    if url.scheme != "https" and not (url.scheme == "http" and loopback):
        raise Refusal("the server URL must be https")
    if url.username or url.query or url.fragment or url.path not in ("", "/"):
        raise Refusal("the server URL must be a plain origin")
    endpoint = f"{url.scheme}://{url.netloc}/v1/server-version"
    try:
        with urllib.request.urlopen(endpoint, timeout=timeout) as response:
            body = json.loads(response.read(65536))
    except (urllib.error.URLError, OSError, ValueError) as error:
        raise Refusal(f"could not read the deployed server version from {endpoint}: {error}")
    commit = (body.get("data") or {}).get("commit") if isinstance(body, dict) else None
    if not isinstance(commit, str) or not COMMIT.match(commit):
        raise Refusal("the deployed server does not report a source commit; deploy a build that does")
    return commit


def git(repo: Path, *args: str) -> str:
    result = subprocess.run(["git", "-C", str(repo), *args], capture_output=True, text=True)
    if result.returncode != 0:
        raise Refusal(result.stderr.strip() or f"git {args[0]} failed")
    return result.stdout


def undeployed(repo: Path, deployed: str, release: str) -> list[str]:
    try:
        git(repo, "cat-file", "-e", deployed + "^{commit}")
    except Refusal:
        raise Refusal(f"the deployed commit {deployed[:12]} is not in this clone; fetch the full history")
    git(repo, "rev-parse", "--verify", release + "^{commit}")
    return git(repo, "log", "--format=%h %s", f"{deployed}..{release}", "--", *PATHS).splitlines()


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--server-url", required=True)
    parser.add_argument("--release", default="HEAD")
    parser.add_argument("--repo", default=str(REPO))
    parser.add_argument("--warn", action="store_true")
    args = parser.parse_args(argv)
    label = "WARNING" if args.warn else "REFUSED"
    try:
        deployed = deployed_commit(args.server_url)
        pending = undeployed(Path(args.repo), deployed, args.release)
    except Refusal as error:
        print(f"{label}: {error}", file=sys.stderr)
        return 0 if args.warn else 1
    if pending:
        print(f"{label}: {len(pending)} server/shared commit(s) in {args.release} are not on the deployed server "
              f"({deployed[:12]}); deploy the server first, then release or push the client:", file=sys.stderr)
        for line in pending[:20]:
            print(f"  {line}", file=sys.stderr)
        return 0 if args.warn else 1
    print(f"OK: the deployed server ({deployed[:12]}) contains every server/ and shared/ commit of {args.release}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
