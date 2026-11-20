#!/usr/bin/env python3
"""Sample Linux production hosts and forward capacity transitions to the canary channel."""
import json
import os
from pathlib import Path
import subprocess
import sys
import time


# One policy for all resources and hosts. Percentages are of the sampled host's capacity.
THRESHOLDS = {"warning": 80, "critical": 90}
HYSTERESIS = 5
SUSTAIN_SECONDS = 180
MAX_SAMPLE_GAP = 90  # the systemd timer runs every minute


def sample():
    stat = Path("/proc/stat").read_text().splitlines()[0].split()
    # guest and guest_nice are already included in user and nice.
    ticks = list(map(int, stat[1:9]))
    mem = {line.split(":")[0]: int(line.split()[1])
           for line in Path("/proc/meminfo").read_text().splitlines()
           if line.startswith(("MemTotal:", "MemAvailable:"))}
    allocated, unused, maximum = map(int, Path("/proc/sys/fs/file-nr").read_text().split())
    disk = subprocess.run(
        ["df", "--local", "--output=size,used,target", "-x", "tmpfs", "-x", "devtmpfs",
         "-x", "overlay", "-x", "squashfs", "-x", "efivarfs"],
        check=True, capture_output=True, text=True, timeout=8,
    ).stdout.splitlines()[1:]
    usage = {"memory": 100 * (mem["MemTotal"] - mem["MemAvailable"]) / mem["MemTotal"],
             "file descriptors": 100 * (allocated - unused) / maximum}
    for line in disk:
        size, used, mount = line.split(maxsplit=2)
        if int(size) > 0:
            usage[f"disk {mount}"] = 100 * int(used) / int(size)
    return {"boot": Path("/proc/sys/kernel/random/boot_id").read_text().strip(),
            "cpu": [sum(ticks), ticks[3] + ticks[4]], "usage": usage}


def cpu_percent(previous, current):
    total = current[0] - previous[0]
    idle = current[1] - previous[1]
    if total > 0 and 0 <= idle <= total:
        return 100 * (total - idle) / total
    return None


def advance(previous, usage, now, continuous):
    """Use independent warning and critical timers, so brief spikes still count toward warning."""
    state = {}
    for level, threshold in THRESHOLDS.items():
        active, since = previous.get(level, (False, None))
        if active:
            active = usage >= threshold - HYSTERESIS
        elif usage >= threshold:
            if since is None or not continuous:
                since = now
            active = now - since >= SUSTAIN_SECONDS
        else:
            since = None
        state[level] = [active, None if active else since]
    return state


def severity(state):
    return next((level for level in reversed(THRESHOLDS) if state.get(level, [False])[0]), "normal")


def assess(previous, current, now):
    continuous = (previous.get("boot") == current["boot"] and
                  0 < now - previous["at"] <= MAX_SAMPLE_GAP)
    usage = dict(current["usage"])
    if continuous and (cpu := cpu_percent(previous["cpu"], current["cpu"])) is not None:
        usage["CPU"] = cpu
    before = previous.get("resources", {})
    # A missing measurement cannot recover a previous alert or preserve a pending threshold.
    resources = {name: {level: [active, None] for level, (active, _) in state.items()}
                 for name, state in before.items() if name == "CPU" and name not in usage}
    transitions = []
    for name, percent in usage.items():
        old = before.get(name, {})
        resources[name] = advance(old, percent, now, continuous)
        level = severity(resources[name])
        if level != severity(old):
            action = "RECOVERED" if level == "normal" else level.upper()
            transitions.append(f"{action}: {name} at {percent:.1f}%")
    return {"at": now, "boot": current["boot"], "cpu": current["cpu"], "resources": resources}, transitions


def read_host(host, script):
    if host == "local":
        return sample()
    result = subprocess.run(
        ["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "--", host, "python3", "-", "--sample"],
        input=script, text=True, capture_output=True, check=True, timeout=12,
    )
    return json.loads(result.stdout)


def main():
    sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "tools/probe"))
    from canary import STATE, now, record_lock, send_alert, write

    hosts = os.environ["CANARY_RESOURCE_HOSTS"].split()
    if not hosts:
        raise ValueError("CANARY_RESOURCE_HOSTS must list the production hosts")
    script = Path(__file__).read_text()
    STATE.mkdir(parents=True, exist_ok=True)
    path = STATE / "host-resources.json"
    previous = json.loads(path.read_text()) if path.exists() else {}
    state, messages = {}, []
    failed = False
    for host in dict.fromkeys(hosts):
        old = previous.get(host, {})
        try:
            current = read_host(host, script)
            state[host], transitions = assess(old, current, time.time())
        except (OSError, subprocess.SubprocessError, ValueError, KeyError, ZeroDivisionError):
            print(f"could not sample host capacity on {host}", file=sys.stderr)
            # Invalidate continuity immediately, even if the next timer fires early.
            state[host] = {**old, "boot": None}
            failed = True
            continue
        messages.extend(f"host capacity {host} {transition} ({now()})" for transition in transitions)
    write(path, json.dumps(state) + "\n")
    if messages:
        with record_lock(), (STATE / "alerts.log").open("a") as log:
            log.write("\n".join(messages) + "\n")
        send_alert("\n".join(messages))
    return int(failed)


if __name__ == "__main__":
    if sys.argv[1:] == ["--sample"]:
        print(json.dumps(sample()))
    else:
        sys.exit(main())
