"""After an update: `pb doctor` is healthy, its machine_guard_version check passes, and every installed Paperboat
component runs the expected version (the coordinator's 11-27 order; the physical Mac's `pb update --from` .21 -> dev left
the guard on .21).

Versions: `pb --version` and `pb update status` (cli_version, runtime_version) name the expected version. Components:
every running Paperboat service process (systemd units paperboat-* and the user's paperboatd; launchd jobs
com.pinksaucepasta.paperboat.*; Windows services Paperboat*) runs the same bytes as the installed pb (SHA-256 of its
executable): hostd, updater, carrier, session owner, host service, machine guard and daemon included.
"""
import json
import time

import pairing_lifecycle as p
import winhost

# Prints "cli <sha>" and "<unit or label> <sha>" per running component. Runs on the machine itself (bash; sudo -n).
UNIX_COMPONENTS = r'''
export XDG_RUNTIME_DIR=${XDG_RUNTIME_DIR:-/run/user/$(id -u)}
h() { if command -v sha256sum >/dev/null 2>&1; then sudo -n sha256sum "$1" | cut -d' ' -f1; else sudo -n shasum -a 256 "$1" | cut -d' ' -f1; fi; }
cli=$(python3 -c 'import os,sys;print(os.path.realpath(sys.argv[1]))' "$HOME/.local/bin/pb")
echo "cli $(h "$cli")"
if command -v systemctl >/dev/null 2>&1; then
  for u in $(systemctl list-units --no-legend --plain --state=running 'paperboat-*' | awk '{print $1}'); do
    pid=$(systemctl show -p MainPID --value "$u"); [ "${pid:-0}" -gt 0 ] && echo "$u $(h /proc/$pid/exe)"
  done
  pid=$(systemctl --user show -p MainPID --value paperboatd.service 2>/dev/null)
  [ "${pid:-0}" -gt 0 ] && echo "paperboatd $(h /proc/$pid/exe)"
else
  { sudo -n launchctl list; launchctl list; } | awk '$3 ~ /^com\.pinksaucepasta\.paperboat/ && $1 ~ /^[0-9]+$/ {print $1, $3}' | sort -u |
    while read -r pid label; do echo "$label $(h "$(ps -o comm= -p "$pid")")"; done
fi
'''

WINDOWS_COMPONENTS = r'''
$cli=(Get-FileHash -Algorithm SHA256 -LiteralPath %s).Hash.ToLower(); "cli $cli"
Get-CimInstance Win32_Service -Filter "Name like 'Paperboat%%'" | Where-Object State -eq 'Running' | ForEach-Object {
  $exe = if ($_.PathName -match '^"([^"]+)"') { $Matches[1] } else { ($_.PathName -split ' ')[0] }
  "$($_.Name) $((Get-FileHash -Algorithm SHA256 -LiteralPath $exe).Hash.ToLower())" }
'''

# A component the check must see running (a substring of its unit, label or service name).
REQUIRED = ('hostd', 'updated', 'carrier')


def components(run_shell=None):
    """{component: sha256} including 'cli'. run_shell(script) -> stdout runs on another machine (a container)."""
    device = winhost.device()
    if device:
        out = device.ps('components', WINDOWS_COMPONENTS % winhost.ps_quote(device.pb_exe), timeout=120)[1]
    elif run_shell:
        out = run_shell(UNIX_COMPONENTS)
    else:
        import subprocess
        out = subprocess.run(['bash', '-c', UNIX_COMPONENTS], capture_output=True, text=True, timeout=120).stdout
    found = {}
    for line in out.splitlines():
        parts = line.split()
        if len(parts) == 2 and len(parts[1]) == 64:
            found[parts[0]] = parts[1]
    return found


def problems(expected, doctor_report, status, version, parts):
    """What is wrong after an update to EXPECTED (empty: healthy and every component on it)."""
    wrong = []
    report = doctor_report.get('data', doctor_report)
    if not p.doctor_healthy(report):
        bad = [c.get('code') + ':' + str(c.get('status')) for c in report.get('checks', []) if c.get('status') not in ('pass', None)]
        wrong.append('doctor %s (%s)' % (report.get('overall'), ', '.join(bad)))
    guard = [c for c in report.get('checks', []) if c.get('code') == 'machine_guard_version']
    if not guard or guard[0].get('status') != 'pass':
        wrong.append('machine_guard_version %s%s' % (guard[0].get('status') if guard else 'missing',
                                                      ': ' + guard[0].get('summary', '') if guard else ''))
    if version != expected:
        wrong.append('pb --version %s' % version)
    for key in ('cli_version', 'runtime_version'):
        if status.get(key) != expected:
            wrong.append('%s %s' % (key, status.get(key)))
    cli = parts.get('cli')
    if not cli:
        wrong.append('no hash of the installed pb')
    others = sorted(k for k in parts if k != 'cli')
    for need in REQUIRED:
        if not any(need in name.lower() for name in others):
            wrong.append('no running %s component' % need)
    stale = ['%s %s' % (name, parts[name][:12]) for name in others if parts[name] != cli]
    if stale:
        wrong.append('not on the installed pb (%s): %s' % ((cli or '?')[:12], '; '.join(stale)))
    return wrong


def settled(expected, seconds, pb=None, status=None, version=None, run_shell=None, clock=time, doctor=None):
    """Waits up to SECONDS until healthy with every component on EXPECTED; returns its receipt, or raises with the doctor
    output and the component list of the last attempt. The callers pass their own readers (local pb or a container)."""
    deadline, last = clock.monotonic() + seconds, None
    while True:
        try:
            report = doctor() if doctor else json.loads(p.command_result(pb, 'doctor', '--json', timeout=90).stdout)
            seen_status, seen_version, parts = status(), version(), components(run_shell)
            wrong = problems(expected, report, seen_status, seen_version, parts)
            last = {'doctor': report.get('data', report), 'components': parts, 'problems': wrong}
            if not wrong:
                return {'version': expected, 'components': sorted(k for k in parts if k != 'cli'), 'doctor': 'healthy'}
        except (RuntimeError, ValueError, KeyError, p.CommandTimeout) as error:  # the daemon restarts during an activation
            last = {'problems': ['%s: %s' % (type(error).__name__, error)]}
        if clock.monotonic() >= deadline:
            raise AssertionError('not healthy on %s after the update: %s; receipt %s'
                                 % (expected, '; '.join(last['problems']), json.dumps(last)[:3000]))
        clock.sleep(10)
