#!/usr/bin/env python3
"""Canary-gated deploy of the server and/or tunnel edges with automatic rollback (deploy/README.md step 6).

Runs on oracle, next to the production canary (tools/probe/canary.py, user unit paperboat-canary):
  0. with --stage CMD: runs it first (deploy the candidate to staging and require a green staging canary,
     deploy/staging.py + deploy/staging-canary.sh); a failing stage refuses the deploy before prod or the lock is touched;
  1. refuses when the canary is red before the deploy (--force "<reason>" overrides);
  2. takes the deploy lock on the control-plane host and holds it to the end;
  3. notes the serving server image and every edge's relay/tunnel images;
  4. runs the deploy command (normally deploy/check-edges.sh --deploy '...', which keeps the 5xx check);
  5. waits up to --wait seconds for one full canary pass that started after the switch;
  6. if the deploy command failed or no pass came, rolls back everything that changed (server first,
     then each edge) to the noted images and runs the canary again to report the result.
Exit: 0 kept, 1 rolled back and green again, 2 rolled back and still red (or rollback failed),
3 refused (canary red before), 4 deploy lock held by someone else, 5 refused (staging failed).
"""
import argparse, json, shlex, subprocess, sys, time
from datetime import datetime, timezone
from pathlib import Path

STATUS = Path.home() / ".local/state/paperboat-canary/status.json"
UNIT = "paperboat-canary.service"
FRESH = 300  # seconds a canary status counts as current; one run a minute, a broken run takes up to ~5 min
PBH = "/usr/local/lib/paperboat-selfhost/pbh"
SERVER_IMAGE = ("docker inspect --format '{{{{.Config.Image}}}}' \"$(python3 -c \"import json;"
                "print(json.load(open('{d}/server-rollout.json'))['active'])\")\"")
EDGE_IMAGES = "sudo -n docker ps --filter name=paperboat-pbh- --format '{{.Names}} {{.Image}}'"


def iso(t):
    return datetime.fromtimestamp(t, timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def parse_edge(text):
    """{'relay': image, 'tunnel': image} from `docker ps` lines of one pbh installation."""
    images = {}
    for line in text.splitlines():
        name, _, image = line.strip().partition(" ")
        for role in ("relay", "tunnel"):
            if name.endswith("-" + role) and image:
                images[role] = image
    return images


class Gate:
    def __init__(self, args, sh, deploy, canary, wall=time.time, mono=time.monotonic, say=print, stage=None):
        self.a, self.sh, self.deploy, self.canary, self.stage = args, sh, deploy, canary, stage
        self.wall, self.mono, self.say = wall, mono, say

    def images(self):
        rc, out = self.sh(self.a.server_host, SERVER_IMAGE.format(d=self.a.directory))
        if rc != 0 or not out.strip():
            raise RuntimeError(f"could not read the serving server image on {self.a.server_host}")
        images = {"server": out.strip()}
        for edge in self.a.edge:
            rc, out = self.sh(edge, EDGE_IMAGES)
            parsed = parse_edge(out)
            if rc != 0 or set(parsed) != {"relay", "tunnel"}:
                raise RuntimeError(f"could not read the relay and tunnel images on edge {edge}")
            images[edge] = parsed
        return images

    def wait_green(self, after):
        """True once a canary run that started at or after `after` (epoch seconds) passes, within --wait."""
        deadline, last = self.mono() + self.a.wait, None
        while self.mono() < deadline:
            last = self.canary()
            run = (last or {}).get("last") or {}
            if (last or {}).get("red") is False and run.get("started", "") >= iso(after) and run.get("ok") is True:
                return True, run
        return False, (last or {}).get("last")

    def rollback(self, before):
        now, ok = self.images(), True
        if now["server"] != before["server"]:
            d = shlex.quote(self.a.directory)
            self.say(f"rollback: server {now['server']} -> {before['server']}")
            rc, out = self.sh(self.a.server_host, f"python3 {d}/rollout-server.py --directory {d} --reap; "
                              f"python3 {d}/rollout-server.py --directory {d} --rollback --image {shlex.quote(before['server'])}")
            ok &= rc == 0 or bool(self.say(f"rollback: server rollout failed (exit {rc}): {out[-300:]}"))
        for edge in self.a.edge:
            if now[edge] != before[edge]:
                b = before[edge]
                self.say(f"rollback: edge {edge} {now[edge]} -> {b}")
                rc, out = self.sh(edge, f"sudo -n {PBH} install --relay-image {shlex.quote(b['relay'])} "
                                  f"--tunnel-image {shlex.quote(b['tunnel'])}")
                ok &= rc == 0 or bool(self.say(f"rollback: edge {edge} failed (exit {rc}): {out[-300:]}"))
        return ok

    def run(self):
        if self.stage is not None and (code := self.stage()) != 0:
            self.say(f"refused: the staging step failed (exit {code}); fix staging or the build first, prod was not touched")
            return 5
        status = read_status(self.a.canary_status)
        if not status or self.wall() - parse_iso(status.get("updated")) > FRESH:
            status = self.canary()
        red = not status or status.get("red") is not False
        if red and not self.a.force:
            failed = [s["step"] for s in ((status or {}).get("last") or {}).get("steps", []) if not s.get("ok")]
            self.say(f"refused: the production canary is red before the deploy ({', '.join(failed) or 'no result'}); "
                     "fix it first, or pass --force '<reason>'")
            return 3
        if red:
            self.say(f"forced past a red canary: {self.a.force}")
        owner = f"{self.a.what} {iso(self.wall())}" + (f" forced: {self.a.force}" if red else "")
        rc, out = self.sh(self.a.server_host, f"set -o noclobber; echo {shlex.quote(owner)} > {self.a.directory}/deploy.lock")
        if rc != 0:
            _, holder = self.sh(self.a.server_host, f"cat {self.a.directory}/deploy.lock")
            self.say(f"deploy lock is held: {holder.strip()}; wait for it, do not remove it")
            return 4
        try:
            before = self.images()
            self.say(f"before: {json.dumps(before)}")
            code = self.deploy()
            switched = self.wall()
            if code != 0:
                reason = f"the deploy command failed (exit {code})"
            else:
                green, run = self.wait_green(switched)
                if green:
                    self.say(f"kept: canary green after the switch (run {run['started']})")
                    return 0
                failed = [s["step"] for s in (run or {}).get("steps", []) if not s.get("ok")]
                reason = f"no green canary run within {self.a.wait} s of the switch (failing: {', '.join(failed) or 'no result'})"
            self.say(f"rolling back: {reason}")
            ok = self.rollback(before)
            green, run = self.wait_green(self.wall())
            self.say(f"after rollback: canary {'green' if green else 'still red'}"
                     + ("" if ok else "; a rollback step failed, see above"))
            return 1 if ok and green else 2
        finally:
            self.sh(self.a.server_host, f"rm -f {self.a.directory}/deploy.lock")


def read_status(path):
    try:
        return json.loads(Path(path).read_text())
    except (OSError, ValueError):
        return None


def parse_iso(text):
    try:
        return datetime.strptime(text, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=timezone.utc).timestamp()
    except (TypeError, ValueError):
        return 0


def shell(target, script):
    command = ["sh", "-c", script] if target == "local" else ["ssh", "-o", "BatchMode=yes", target, script]
    try:
        done = subprocess.run(command, capture_output=True, text=True, timeout=900)
        return done.returncode, done.stdout + done.stderr
    except subprocess.TimeoutExpired:
        return 124, f"timed out: {script[:120]}"


def main(argv=None):
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--what", required=True, help="lane and what is deployed, written into the deploy lock")
    p.add_argument("--stage", default="", help="staging step, run with sh -c before anything else; must exit 0")
    p.add_argument("--deploy", required=True, help="the deploy command, run with sh -c on this host")
    p.add_argument("--server-host", required=True, help="ssh target of the control-plane host")
    p.add_argument("--directory", default="/opt/paperboat", help="deployment directory on the control-plane host")
    p.add_argument("--edge", action="append", default=[], help="edge host: 'local' or an ssh target (repeatable)")
    p.add_argument("--wait", type=int, default=300, help="seconds to wait for a green canary after the switch")
    p.add_argument("--force", default="", help="deploy although the canary is red; the reason is required")
    p.add_argument("--canary-status", default=str(STATUS))
    a = p.parse_args(argv)

    def canary():
        subprocess.run(["systemctl", "--user", "start", UNIT], timeout=420, capture_output=True)
        return read_status(a.canary_status)

    def deploy():
        return subprocess.run(["sh", "-c", a.deploy]).returncode

    def stage():
        return subprocess.run(["sh", "-c", a.stage]).returncode

    try:
        return Gate(a, shell, deploy, canary, stage=stage if a.stage else None).run()
    except RuntimeError as error:  # the lock is already released; nothing was rolled back
        print(f"gate stopped: {error}")
        return 2


if __name__ == "__main__":
    sys.exit(main())
