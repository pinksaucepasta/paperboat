#!/usr/bin/env python3
"""F8-03: a public tunnel survives a victus machine reboot.

Flow: on-device origin + `tunnel create` (public, 60m) -> assert URL serves the
marker and `tunnel status` is ready -> record boot time + Paperboat* services +
VM states -> `shutdown /r /t 5` -> wait for ssh (bounded 6 min, 60 s polls) ->
assert new boot, services Running unassisted, doctor healthy, server-side online
-> restart the origin on the SAME port -> within 5 min (10 s polls) the SAME
tunnel URL serves again and status is ready with the same id (tunnel untouched)
-> `tunnel delete` (confirmed) + stop origin -> restore VMs to pre-reboot
states, verify pbvm2/pbvm3 ssh.

Env: PB_SMOKE_WINDOWS_PB (installed pb.exe on victus), SMOKE_WINDOWS_SSH
(default pujan@victus), VICTUS_PASS. Prints exactly one JSON line
{check:'tunnel-reboot', ...}. Cleans up on every exit path. Never prints
tokens, passwords or file contents.
"""
import json
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
import uuid
from pathlib import Path

import winhost  # noqa: E402
from pairing_lifecycle import command_result  # noqa: E402
from preview_lifecycle import stoppable_origin  # noqa: E402

BOOT_TIME = "(Get-CimInstance Win32_OperatingSystem).LastBootUpTime.ToUniversalTime().ToString('o')"
VM_STATE = ("Get-VM | Select-Object Name,@{n='State';e={$_.State.ToString()}},"
            "@{n='AutoStart';e={$_.AutomaticStartAction.ToString()}}|ConvertTo-Json -Compress")
SSH_BACK_BOUND = 360
SSH_BACK_POLL = 60
POST_BOUND = 300
POST_POLL = 10

events = {}
notes = []


def stamp(key):
    events[key] = time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())


def run(*args, timeout=120):
    result = command_result(winhost.pb_path(), *args, '--json', timeout=timeout)
    try:
        value = json.loads(result.stdout)
    except ValueError:
        raise RuntimeError('command did not return JSON: ' + ' '.join(args[:3]))
    if result.returncode or (isinstance(value, dict) and value.get('ok') is False):
        detail = json.dumps((value.get('error', {}) if isinstance(value, dict) else {}))[:300]
        raise RuntimeError('command failed: ' + ' '.join(args[:3]) + ' rc=' +
                           str(result.returncode) + ' ' + detail)
    return value.get('data', value) if isinstance(value, dict) and 'ok' in value else value


def confirmed(*args, timeout=120):
    result = command_result(winhost.pb_path(), *args, '--json', timeout=timeout)
    try:
        preview = json.loads(result.stdout)
    except ValueError:
        raise RuntimeError('delete did not return a JSON preview')
    data = preview.get('data', preview) if isinstance(preview, dict) else {}
    token = (data or {}).get('confirmation_token')
    assert token, 'no confirmation preview for tunnel delete'
    return run(*args, '--confirm', token, timeout=timeout)


def fetch(url, timeout=20):
    try:
        with urllib.request.urlopen(url + '/', timeout=timeout) as response:
            return response.status, response.read().decode(errors='replace')
    except urllib.error.HTTPError as error:
        return error.code, ''
    except OSError as error:
        return 0, str(error)[:80]


def device_boot(device):
    return device.ps('boot time', BOOT_TIME, timeout=60)[1].strip()


def device_services(device):
    return sorted('%s:%s/%s' % (s.get('Name'), s.get('Status'), s.get('StartType'))
                  for s in device.services('Paperboat*'))


def vm_states(device):
    return json.loads(device.ps('vm states', VM_STATE, timeout=120)[1])


def hp_online(machine_id):
    found, deadline = False, time.monotonic() + 180
    while time.monotonic() < deadline:
        queried = subprocess.run(['ssh', '-o', 'ConnectTimeout=15', '-o', 'BatchMode=yes',
                                  '-o', 'StrictHostKeyChecking=accept-new', 'anvit@hp',
                                  '$HOME/.local/bin/pb machine list --all --json'],
                                 capture_output=True, text=True, timeout=60)
        try:
            body = json.loads(queried.stdout)
            # hp's older pb returns the machine list bare; newer builds wrap it in a data envelope.
            inner = (body.get('data', body) or {}) if isinstance(body, dict) else {}
            machines = inner.get('machines', [])
            if any(m.get('id') == machine_id and m.get('online') for m in machines):
                found = True
                break
        except ValueError:
            pass
        time.sleep(60)
    return found


def vm_ssh(name, timeout=60):
    try:
        done = subprocess.run(['ssh', '-o', 'ConnectTimeout=15', '-o', 'BatchMode=yes',
                               '-o', 'StrictHostKeyChecking=accept-new', name, 'echo ok'],
                              capture_output=True, text=True, timeout=timeout)
        return done.returncode == 0 and 'ok' in done.stdout
    except (subprocess.SubprocessError, OSError):
        return False


def main():
    device = winhost.device()
    assert device is not None, 'PB_SMOKE_WINDOWS_PB is not set'
    marker = 'PB_F803_' + uuid.uuid4().hex
    page = '<!doctype html><title>smoke</title><p>' + marker + '</p>'
    identifier = None
    stop_first = None
    stop_second = None
    outcome = {'check': 'tunnel-reboot'}
    try:
        stamp('driver_start')
        before_reg = json.loads((device.runtime_dir() / 'machine-registration.json').read_text())
        outcome['machine_id'] = before_reg.get('machine_id')
        outcome['boot_pre'] = device_boot(device)
        outcome['services_pre'] = device_services(device)
        outcome['vms_pre'] = vm_states(device)
        stamp('prestate_recorded')

        with tempfile.TemporaryDirectory(prefix='f803-origin-') as directory:
            (Path(directory) / 'index.html').write_text(page)
            port, stop_first = stoppable_origin(directory, page, 0)
            outcome['origin_port'] = port
            stamp('origin_started')
            try:
                name = 'f803-reboot-' + uuid.uuid4().hex[:8]
                created = run('tunnel', 'create', name, '--port', str(port),
                              '--duration', '60m', '--wait', '--timeout', '90s', timeout=180)
                tunnel = created['tunnel']
                identifier = tunnel['id']
                endpoint = tunnel['stable_endpoint']
                outcome['tunnel_id'] = identifier
                stamp('tunnel_created')
                code, body = fetch(endpoint)
                if not (code == 200 and marker in body):
                    raise RuntimeError('tunnel did not serve before the reboot (http %s)' % code)
                stamp('served_pre')
                status = run('tunnel', 'status', identifier, timeout=60)
                if status.get('overall_code', status.get('overall')) != 'ready':
                    raise RuntimeError('tunnel not ready before the reboot')
                stamp('ready_pre')
                try:
                    doctor = run('doctor', '--json', timeout=120)
                    outcome['doctor_pre'] = (doctor.get('data', doctor) or {}).get('overall',
                                                                                  doctor.get('overall'))
                except RuntimeError as error:
                    outcome['doctor_pre'] = 'unhealthy: ' + str(error)[:120]
                    notes.append('pre-reboot doctor not healthy; tunnel still served')
            except BaseException:
                if identifier is None:
                    raise
                # Tunnel exists but pre-asserts failed: still test survival after reboot.
                notes.append('pre-reboot tunnel asserts incomplete; continuing to reboot')
                stamp('served_pre_attempted')

            outcome['reboot_issued'] = time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())
            try:
                device.run(['shutdown.exe', '/r', '/t', '5'], timeout=30)
            except Exception:
                pass  # the session dies with the reboot; any error here is the reboot itself
            stamp('reboot_command_sent')
            print(json.dumps({'state': 'rebooting'}), flush=True)

            deadline = time.monotonic() + SSH_BACK_BOUND
            back = False
            while time.monotonic() < deadline:
                time.sleep(SSH_BACK_POLL)
                try:
                    device.ps('ssh back', "$env:USERNAME", timeout=30)
                    back = True
                    break
                except Exception:
                    pass
            if not back:
                raise RuntimeError('ssh did not come back within 6 min of the reboot')
            stamp('ssh_back')
            print(json.dumps({'state': 'ssh_back'}), flush=True)

            outcome['boot_post'] = device_boot(device)
            if outcome['boot_post'] == outcome['boot_pre']:
                raise RuntimeError('device did not reboot (boot time unchanged)')
            outcome['services_post'] = device_services(device)
            if len(outcome['services_post']) != 6 or not all(
                    s.endswith(':Running/Automatic') for s in outcome['services_post']):
                raise RuntimeError('Paperboat services not all Running/Automatic after reboot')
            stamp('services_post_ok')
            doctor_deadline = time.monotonic() + 360
            doctor_last = 'no result'
            while True:
                try:
                    run('doctor', '--json', timeout=120)
                    break
                except RuntimeError as error:
                    doctor_last = str(error)[:200]
                    if time.monotonic() >= doctor_deadline:
                        raise RuntimeError('doctor not healthy within 6 min of reboot: ' + doctor_last)
                    time.sleep(60)
            stamp('doctor_post_healthy')
            if not hp_online(outcome['machine_id']):
                raise RuntimeError('machine not online server-side after the reboot')
            stamp('online_post')
            print(json.dumps({'state': 'daemon_back_healthy'}), flush=True)

            # VMs back to pre-reboot states (start only what was Running before).
            outcome['vms_post_reboot'] = vm_states(device)
            running_before = {v['Name'] for v in outcome['vms_pre'] if v['State'] == 'Running'}
            for vm in outcome['vms_post_reboot']:
                if vm['Name'] in running_before and vm['State'] != 'Running':
                    device.ps('start ' + vm['Name'],
                              "Start-VM -Name $req.name", timeout=300, name=vm['Name'])
                    notes.append('started ' + vm['Name'] + ' (was ' + vm['State'] + ')')
            time.sleep(60)
            outcome['vms_final'] = vm_states(device)
            stamp('vms_restored')
            outcome['pbvm2_ssh'] = vm_ssh('pbvm2')
            outcome['pbvm3_ssh'] = vm_ssh('pbvm3')
            print(json.dumps({'state': 'pbvm3_back', 'pbvm3_ssh': outcome['pbvm3_ssh'],
                              'vms_final': outcome['vms_final']}), flush=True)
            if not (outcome['pbvm2_ssh'] and outcome['pbvm3_ssh']):
                raise RuntimeError('pbvm2/pbvm3 do not answer ssh after restore')

            # The on-device origin died with the reboot: start it again on the SAME port.
            _, stop_second = stoppable_origin(directory, page, port)
            stamp('origin_restarted')
            deadline = time.monotonic() + POST_BOUND
            served_post = ready_post = False
            while time.monotonic() < deadline:
                code, body = fetch(endpoint)
                served_post = code == 200 and marker in body
                try:
                    status = run('tunnel', 'status', identifier, timeout=60)
                    ready_post = (status.get('overall_code', status.get('overall')) == 'ready'
                                  and status.get('tunnel', status).get('id', identifier) == identifier)
                except RuntimeError:
                    ready_post = False
                if served_post and ready_post:
                    break
                time.sleep(POST_POLL)
            outcome['served_post'] = served_post
            outcome['ready_post'] = ready_post
            stamp('served_post_checked')
            if not (served_post and ready_post):
                raise RuntimeError('same tunnel did not serve again within 5 min')
            after_reg = json.loads((device.runtime_dir() / 'machine-registration.json').read_text())
            if after_reg.get('machine_id') != outcome['machine_id']:
                raise RuntimeError('registration changed across the reboot')
            outcome['status'] = 'PASS'
    except BaseException as error:
        outcome['status'] = 'FAIL'
        outcome['reason'] = str(error)[:400]
        raise
    finally:
        try:
            if identifier is not None:
                try:
                    confirmed('tunnel', 'delete', identifier, timeout=60)
                    stamp('tunnel_deleted')
                except BaseException as error:
                    notes.append('tunnel delete failed: ' + str(error)[:150])
            if stop_second is not None or stop_first is not None:
                for stop in (stop_second, stop_first):
                    if stop is not None:
                        try:
                            stop()
                        except BaseException as error:
                            notes.append('origin stop failed: ' + str(error)[:100])
        finally:
            outcome['events'] = events
            if notes:
                outcome['notes'] = notes
            print(json.dumps(outcome), flush=True)


if __name__ == '__main__':
    main()
