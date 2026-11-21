#!/usr/bin/env python3
"""Stop foreign-config pb daemons inside a dedicated Linux canary container."""

import os
import select
import signal
from pathlib import Path

OWNED_CONFIG = "/home/pbt/.config/paperboat/config.json"


def daemon_config(argv):
    if not argv or os.path.basename(argv[0]) != "pb":
        return None
    args = argv[1:]
    # Restrict the sweep to the daemon entry point with an explicit config.
    # Managed internal roles and one-shot daemon subcommands are left alone.
    for command in (["daemon", "run"], ["daemon"]):
        if args[:len(command)] == command:
            args = args[len(command):]
            break
        if args[-len(command):] == command:
            args = args[:-len(command)]
            break
    else:
        return None
    if len(args) == 2 and args[0] == "--config":
        return args[1] or None
    if len(args) == 1 and args[0].startswith("--config="):
        return args[0].removeprefix("--config=") or None
    return None


def stop_unowned_daemon(pid, owned_config):
    try:
        handle = os.pidfd_open(pid)
    except ProcessLookupError:
        return
    try:
        proc = Path("/proc") / str(pid)
        argv = os.fsdecode((proc / "cmdline").read_bytes()).split("\0")
        config = daemon_config(argv[:-1])
        if config is None:
            return
        if not os.path.isabs(config):
            config = os.path.join(os.readlink(proc / "cwd"), config)
        if os.path.realpath(config) == os.path.realpath(owned_config):
            return

        poll = select.poll()
        poll.register(handle, select.POLLIN)
        signal.pidfd_send_signal(handle, signal.SIGTERM)
        if not poll.poll(1000):
            signal.pidfd_send_signal(handle, signal.SIGKILL)
            if not poll.poll(1000):
                raise RuntimeError(f"foreign canary daemon PID {pid} did not exit")
    except (FileNotFoundError, ProcessLookupError):
        pass
    finally:
        os.close(handle)


if __name__ == "__main__":
    for entry in Path("/proc").iterdir():
        if entry.name.isdecimal():
            stop_unowned_daemon(int(entry.name), OWNED_CONFIG)
