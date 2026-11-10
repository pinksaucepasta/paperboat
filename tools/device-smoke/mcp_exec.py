#!/usr/bin/env python3
"""MCP_SMOKE_EXEC for a Windows source (fresh_pair_mcp.py --windows-target): `mcp_exec.py NAME ARGV...`.

NAME ending in -c is the source: ARGV runs on the Windows device over ssh with this process's standard streams
(`pb mcp` speaks MCP on them). Any other NAME is the target: ARGV runs in the container MCP_EXEC_TARGET on the
docker host MCP_EXEC_HOST, as mcp-smoke itself does for a container.
"""
import os
import shlex
import subprocess
import sys

import winhost


def command(name, argv):
    """(argv, environment) that runs ARGV on the side NAME."""
    if name.endswith('-c'):
        device = winhost.device()
        return device.ssh_prefix(device.target, subprocess.list2cmdline(argv)), device.ssh_environment() or os.environ
    inside = ['sudo', '-n', 'docker', 'exec', '-i', '-u', 'pbt', '-e', 'HOME=/home/pbt', '-e',
              'XDG_RUNTIME_DIR=/run/user/1000', os.environ['MCP_EXEC_TARGET'], *argv]
    return ['ssh', '-o', 'BatchMode=yes', '-o', 'ConnectTimeout=10', os.environ['MCP_EXEC_HOST'], shlex.join(inside)], os.environ


if __name__ == '__main__':
    argv, environment = command(sys.argv[1], sys.argv[2:])
    os.execvpe(argv[0], argv, environment)
