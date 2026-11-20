#!/usr/bin/env python3
"""Run every module's Go tests on our own hosts in parallel instead of waiting for GitHub's one Linux CI run.

  tools/linux-ci.py [--dry] [--no-race] [--hosts linpool-5,linpool-6,ana@hermes]

Jobs: the cli packages split like tools/ci-shards.py (Linux seconds in tools/ci-shard-weights-linux.json), relay, edge, shared, helper,
server (PostgreSQL). Each host runs its queue one job at a time through tools/remote-test.sh (it holds a per-host lock and caps memory);
hosts run in parallel. cli needs passwordless sudo and server needs PostgreSQL, so they never land on a host without them.
Prints one line per job (host, seconds, result, failing tests) and the wall clock; exit 1 if any job failed.
Not covered here (GitHub's CI still runs them): policy and static-analysis checks, the cli tests under sudo (tools/cli-root-tests.sh), console and admin.
"""
import argparse, importlib.util, json, re, subprocess, sys, threading, time
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
spec = importlib.util.spec_from_file_location("ci_shards", ROOT / "tools/ci-shards.py")
shards = importlib.util.module_from_spec(spec)
spec.loader.exec_module(shards)

DEFAULT_HOSTS = ["linpool-5", "linpool-6", "ana@hermes"]
NO_SUDO = {"ana@hermes"}  # cli root-updater and isolated-filesystem tests need passwordless sudo
NO_SERVER = {"ana@hermes"}  # the server -race suite ran ~48 min there (6 GB RAM, 2026-11-20); the linpool hosts have 16 GB
SMALL = {"relay": 90, "edge": 130, "shared": 40, "helper": 20, "server": 600}  # seconds of work (CI test steps, 2026-11-20)
CLI_SHARDS = 1  # one job: with -p 2 the whole cli module is ~670 s and the server suite fills the other sudo host
PARALLEL = 2  # remote-test.sh runs go test -p 2: a shard takes about half its summed package seconds (but never less than its slowest package)


def jobs():
    weight = shards.weigh(shards.packages(), json.loads((ROOT / "tools/ci-shard-weights-linux.json").read_text()))
    out = [("cli", pkgs, max(seconds // PARALLEL, max(weight[p] for p in pkgs))) for seconds, pkgs in shards.split_even(weight, CLI_SHARDS)]
    return out + [(module, ["./..."], seconds) for module, seconds in SMALL.items()]


def assign(job_list, hosts):
    """cli first (it needs sudo), then the rest longest first, each onto the host with the least queued seconds."""
    queues = {host: [0, []] for host in hosts}
    for module, pkgs, seconds in sorted(job_list, key=lambda j: (j[0] != "cli", -j[2])):
        allowed = [h for h in hosts if not (module == "cli" and h in NO_SUDO) and not (module == "server" and h in NO_SERVER)]
        host = min(allowed, key=lambda h: queues[h][0])
        queues[host][0] += seconds
        queues[host][1].append((module, pkgs, seconds))
    return {host: queue for host, (_, queue) in queues.items()}


def reachable(hosts):
    """Hosts that answer ssh now: a linpool runner's tunnel can be down (22405/22406 refused), which failed jobs at start."""
    up = []
    for host in hosts:
        ok = subprocess.run(["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", host, "true"], capture_output=True).returncode == 0
        print(f"{host}: {'up' if ok else 'UNREACHABLE, not used'}", flush=True)
        up.append(host) if ok else None
    return up


def run(host, module, pkgs, race, results, index, logdir):
    args = [str(ROOT / "tools/remote-test.sh"), host, module, *(["-race"] if race else []), "-timeout", "20m", *pkgs]
    start = time.time()
    done = subprocess.run(args, capture_output=True, text=True)
    text = done.stdout + done.stderr
    failing = sorted(set(re.findall(r"^--- FAIL: (\S+)", text, re.M) + re.findall(r"^FAIL\s+(\S+)", text, re.M)))
    (logdir / f"{index}-{module}.log").write_text(text)
    results[index] = (host, module, len(pkgs), int(time.time() - start), done.returncode, failing[:6])
    print(f"{host} {module} ({len(pkgs)} pkgs): {'ok' if done.returncode == 0 else 'FAIL'} in {results[index][3]}s" + (f" failing: {failing[:6]}" if failing else ""), flush=True)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--dry", action="store_true")
    parser.add_argument("--no-race", action="store_true")
    parser.add_argument("--hosts", default=",".join(DEFAULT_HOSTS))
    args = parser.parse_args()
    hosts = args.hosts.split(",") if args.dry else reachable(args.hosts.split(","))
    if not any(h not in NO_SUDO for h in hosts):
        print("no reachable host with passwordless sudo for cli", file=sys.stderr)
        return 1
    plan = assign(jobs(), hosts)
    for host, queue in plan.items():
        print(f"{host}: " + ", ".join(f"{m}({len(p)} pkgs, ~{s}s)" for m, p, s in queue), flush=True)
    if args.dry:
        return 0
    logdir = Path(f"/tmp/linux-ci-{int(time.time())}")  # one directory per run: concurrent runs overwrote each other's logs
    logdir.mkdir()
    print(f"logs in {logdir}", flush=True)
    start, results, threads, index = time.time(), {}, [], 0

    def worker(host, queue, first):
        for offset, (module, pkgs, _) in enumerate(queue):
            run(host, module, pkgs, not args.no_race, results, first + offset, logdir)

    for host, queue in plan.items():
        thread = threading.Thread(target=worker, args=(host, queue, index))
        index += len(queue)
        thread.start()
        threads.append(thread)
    for thread in threads:
        thread.join()
    bad = False
    for _, (host, module, count, seconds, rc, failing) in sorted(results.items()):
        print(f"{host} {module} ({count} pkgs): {'ok' if rc == 0 else 'FAIL'} in {seconds}s" + (f" failing: {failing}" if failing else ""))
        bad = bad or rc != 0
    print(f"linux ci: {'FAIL' if bad else 'PASS'} in {int(time.time() - start)}s", flush=True)
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main())
