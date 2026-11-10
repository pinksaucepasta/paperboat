#!/usr/bin/env python3
"""win_console_fixtures.py FIXTURE [COMMAND...]: the Windows counterpart of mac_console_fixtures.sh, run on the controller.

Holds a fixture on the Windows device (winhost: PB_SMOKE_WINDOWS_PB, SMOKE_WINDOWS_SSH, VICTUS_PASS) while COMMAND runs
here with the fixture's values in its environment, then removes the fixture by exact id. Exits with COMMAND's status.
  origin           an HTTP origin on the device's loopback                    PORT
  preview          origin + background preview                                PORT PREVIEW_ID PREVIEW_HOST
  tunnel           origin + tunnel                                            PORT TUNNEL_ID TUNNEL_NAME
  restart-carrier  restart the carrier service (Linux: systemctl restart "paperboat-carrier-u<uid>-*.service"); no COMMAND
  block, unblock   cut every running pb.exe off the network with a firewall rule, and remove the rule; no COMMAND
  stop-preview ID  pb preview stop ID on the device; no COMMAND
"""
import json
import os
import subprocess
import sys
import time

import winhost


RULE = 'pbsmoke-console-block'
ONCE = {  # one PowerShell command each, no COMMAND
    'restart-carrier': "Restart-Service -Name 'PaperboatCarrier-*' -Force",
    # Every running pb.exe loses its outbound traffic (Linux: an iptables rule in the container, macOS: a pf anchor).
    'block': "Get-Process pb | Select-Object -ExpandProperty Path | Sort-Object -Unique | ForEach-Object { "
             "New-NetFirewallRule -DisplayName '%s' -Direction Outbound -Action Block -Program $_ | Out-Null }" % RULE,
    'unblock': "Remove-NetFirewallRule -DisplayName '%s' -ErrorAction SilentlyContinue" % RULE,
}


def pb(device, *arguments, ok=(0,), timeout=180):
    done = device.run([device.pb_exe, *arguments, '--json'], timeout=timeout)
    if done.returncode not in ok:
        sys.exit('FAIL pb %s (exit %s)' % (' '.join(arguments[:2]), done.returncode))
    data = json.loads(done.stdout)
    return data.get('data', data)


def main():
    device, fixture, command = winhost.device(), sys.argv[1], sys.argv[2:]
    if fixture in ONCE:
        device.ps(fixture, ONCE[fixture], timeout=120)
        return 0
    if fixture == 'stop-preview':
        pb(device, 'preview', 'stop', command[0])
        return 0
    with device.origin('paperboat console fixture') as port:
        values, undo = {'PORT': str(port)}, None
        try:
            if fixture == 'preview':
                made = pb(device, 'preview', str(port), '--background', '--ttl', '30m')
                values.update(PREVIEW_ID=made['id'], PREVIEW_HOST=made['endpoint'].split('//')[1])
                undo = lambda: device.run([device.pb_exe, 'preview', 'stop', made['id'], '--json'])
            elif fixture == 'tunnel':
                name = 'final-win-%d' % time.time()
                made = pb(device, 'tunnel', 'create', name, '--port', str(port), '--wait', '--timeout', '90s')['tunnel']
                values.update(TUNNEL_ID=made['id'], TUNNEL_NAME=name)
                undo = lambda: pb(device, 'tunnel', 'delete', made['id'], '--confirm',
                                  pb(device, 'tunnel', 'delete', made['id'], ok=(0, 2))['confirmation_token'])
            elif fixture != 'origin':
                sys.exit(__doc__)
            return subprocess.run(command, env=dict(os.environ, **values)).returncode
        finally:
            if undo:
                undo()


if __name__ == '__main__':
    sys.exit(main())
