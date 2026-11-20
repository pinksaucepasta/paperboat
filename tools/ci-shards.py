#!/usr/bin/env python3
"""Split the cli packages into shards balanced by recorded test seconds and run one cross-platform workflow per shard at once.

  tools/ci-shards.py [--shards 5] [--ref main] [--weights tools/ci-shard-weights.json] [--dry] [--timeout 1500]

A cross-platform dispatch has its own concurrency group, so shards run in parallel; each takes the `packages` list of its shard
and shared=false (the shared module and the harness unit tests run once, with shard 1). The packages are listed from the cli
module (those with test files). A package missing from the weights file weighs DEFAULT seconds.
Waits for every run and prints one line per shard: packages, result, wall seconds, failing jobs. Exit 1 if any shard failed.
The weights file maps a cli package path (./internal/x) to its slowest seconds over the macOS and Windows jobs (median of three runs, 2026-11-20);
the packages it does not list took about 1.2 s each. Shard 1 also runs go build and go vet (~230 s), so it starts with that load: the other shards skip them.
"""
import argparse, json, subprocess, sys, time
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
WORKFLOW = "cross-platform.yml"
DEFAULT = 2  # seconds of a package not in the weights file
FIRST_SHARD_OVERHEAD = 230  # go build + go vet, which only the first shard runs


def packages():
    """Relative paths (./internal/x) of the cli packages that have tests, with their test-file count."""
    listing = subprocess.run(["go", "list", "-f", "{{.Dir}} {{len .TestGoFiles}} {{len .XTestGoFiles}}", "./..."], cwd=ROOT / "cli",
                             capture_output=True, text=True, check=True).stdout
    found = {}
    for line in listing.splitlines():
        directory, tests, external = line.rsplit(" ", 2)
        files = int(tests) + int(external)
        if files:
            found["./" + str(Path(directory).relative_to(ROOT / "cli"))] = files
    return found


def weigh(found, weights):
    return {pkg: weights.get(pkg, DEFAULT) for pkg in found}


def split(weight, count):
    """Longest-processing-time first: each package goes to the lightest shard. Returns [(seconds, [packages])]."""
    shards = [[FIRST_SHARD_OVERHEAD if n == 0 else 0, []] for n in range(count)]
    for pkg in sorted(weight, key=lambda p: (-weight[p], p)):
        lightest = min(shards, key=lambda s: s[0])
        lightest[0] += weight[pkg]
        lightest[1].append(pkg)
    return [(seconds, sorted(pkgs)) for seconds, pkgs in shards if pkgs]  # shards[0] keeps its overhead in the seconds


def split_even(weight, count):
    """split() without the first-shard overhead: for runs that have no go build or go vet in one of them."""
    shards_ = [[0, []] for _ in range(count)]
    for pkg in sorted(weight, key=lambda p: (-weight[p], p)):
        lightest = min(shards_, key=lambda s: s[0])
        lightest[0] += weight[pkg]
        lightest[1].append(pkg)
    return [(seconds, sorted(pkgs)) for seconds, pkgs in shards_ if pkgs]


def gh(*args):
    return subprocess.run(["gh", *args], capture_output=True, text=True, check=True).stdout


def dispatch(ref, label, pkgs, first):
    gh("workflow", "run", WORKFLOW, "--ref", ref, "-f", "packages=" + " ".join(pkgs), "-f", "shard=" + label,
       "-f", "shared=" + ("true" if first else "false"))


def find_runs(labels, since, wait=120):
    """Run ids by shard label (the run title is 'Cross-platform <label>')."""
    found, deadline = {}, time.time() + wait
    while len(found) < len(labels) and time.time() < deadline:
        runs = json.loads(gh("run", "list", "--workflow", WORKFLOW, "--event", "workflow_dispatch", "--limit", "40",
                             "--json", "databaseId,displayTitle,createdAt"))
        for run in runs:
            title = run["displayTitle"]
            if run["createdAt"] >= since and title.startswith("Cross-platform ") and title[15:] in labels:
                found[title[15:]] = run["databaseId"]
        time.sleep(3)
    return found


def result(run_id):
    data = json.loads(gh("run", "view", str(run_id), "--json", "status,conclusion,startedAt,updatedAt,jobs"))
    failed = [j["name"] for j in data["jobs"] if j.get("conclusion") not in (None, "success", "skipped")]
    return data["status"], data.get("conclusion"), failed


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--shards", type=int, default=5)
    parser.add_argument("--ref", default="main")
    parser.add_argument("--weights", default=str(ROOT / "tools/ci-shard-weights.json"))
    parser.add_argument("--dry", action="store_true", help="print the shards, dispatch nothing")
    parser.add_argument("--timeout", type=int, default=1500)
    args = parser.parse_args()
    weights = json.loads(Path(args.weights).read_text()) if Path(args.weights).exists() else {}
    shards = split(weigh(packages(), weights), args.shards)
    for number, (seconds, pkgs) in enumerate(shards, 1):
        print(f"shard {number}: {len(pkgs)} packages, ~{seconds}s", flush=True)
    if args.dry:
        for number, (_, pkgs) in enumerate(shards, 1):
            print(f"shard {number}: {' '.join(pkgs)}")
        return 0
    start = time.time()
    since = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(start - 5))
    labels = {}
    for number, (_, pkgs) in enumerate(shards, 1):
        label = f"shard-{number}-of-{len(shards)}-{int(start)}"
        labels[label] = number
        dispatch(args.ref, label, pkgs, number == 1)
    runs = find_runs(set(labels), since)
    missing = set(labels) - set(runs)
    if missing:
        print("never saw the runs of", sorted(missing), file=sys.stderr)
        return 1
    done, bad = {}, False
    while len(done) < len(runs) and time.time() - start < args.timeout:
        for label, run_id in runs.items():
            if label in done:
                continue
            status, conclusion, failed = result(run_id)
            if status == "completed":
                done[label] = (conclusion, failed)
                print(f"shard {labels[label]}: {conclusion} after {int(time.time() - start)}s run {run_id}" + (f" failed: {failed}" if failed else ""), flush=True)
                bad = bad or conclusion != "success"
        time.sleep(15)
    for label in set(runs) - set(done):
        print(f"shard {labels[label]}: still running after {args.timeout}s run {runs[label]}", file=sys.stderr)
        bad = True
    print(f"all shards: {'FAIL' if bad else 'PASS'} in {int(time.time() - start)}s")
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
