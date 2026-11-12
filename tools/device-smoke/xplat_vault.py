#!/usr/bin/env python3
"""X-13: the ENV vault across OSes — a vault made on one OS opens on the others, lock and reset are honoured, no value leaks.

  xplat_vault.py [--devices L,M,W] [--only lock,remove,persist,leak,reset]     (fresh-account fleet, vault password in ~/.pbmac/xplat2-vault.pw, recovery code in
                                        ~/.pbmac/xplat2-recovery; both read inside this script, never printed)

Per device: lock -> the ENV is not delivered/readable; unlock with the password; lock -> unlock with the recovery code.
Across devices: a vault reset on one device makes every other device refuse the old password and open with the new one
(then it is put back). Finally the sentinel values set by xplat_env.py are searched for in each device's pb config and
diagnostics, shell history and pb logs: they must appear nowhere.
Results: tools/device-smoke/results/xplat-vault-<stamp>.json (`X-13:lock:L`, `X-13:reset:L>W`, `X-13:leak:W`).
"""
import json
import os
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
import xplat_fleet as fleet  # noqa: E402
import xplat_env as env  # noqa: E402

results = {}
PASSWORD = Path(os.environ.get('XPLAT_VAULT_PW', Path.home() / '.pbmac/xplat2-vault.pw'))
RECOVERY = Path(os.environ.get('XPLAT_VAULT_RECOVERY', Path.home() / '.pbmac/xplat2-recovery'))

SEARCH = r'''
import os, sys
needles = [n.encode() for n in sys.argv[2:]]
hits = []
home = os.path.expanduser("~")
roots = [r for r in sys.argv[1].split(os.pathsep) if r]
for root in roots:
    for d, _, files in os.walk(root):
        for f in files:
            p = os.path.join(d, f)
            try:
                if os.path.getsize(p) > 64 << 20:
                    continue
                data = open(p, "rb").read()
            except OSError:
                continue
            for n in needles:
                if n in data or n.decode("utf-8", "ignore").encode("utf-16-le") in data:
                    hits.append(p)
print("\n".join(sorted(set(hits))))
'''


def py(dev):
    return 'python' if dev.os == 'windows' else 'python3'


def unlock(dev, secret):
    return dev.pb('env', 'vault', 'unlock', '--password-stdin', input=secret + b'\n', timeout=120)


def locked(dev):
    """With the vault locked an ENV command must say so instead of delivering values."""
    done = dev.pb('env', 'list', timeout=120)
    return done.rc != 0 and 'lock' in (done.text + done.errtext).lower()


def recover_and_restore(dev):
    """`pb env vault recover` with the recovery code gives a new password and recovery code; the account's files are rewritten
    with them (mode 600, never printed) so the later checks keep working."""
    recovery = RECOVERY.read_bytes().strip()
    new_password = b'rec-' + os.urandom(12).hex().encode()
    stage = dev.home + ('\\xplat-rec' if dev.os == 'windows' else '/.xplat-rec')
    old_file = stage + ('\\old' if dev.os == 'windows' else '/old')
    dev.write(old_file, recovery, mode='600')
    if dev.os == 'windows':
        import winhost
        winhost.RemotePath(dev.device, old_file).enrollment_token_file()  # the owner-only ACL pb requires of a secret file
    new_file = stage + ('\\new' if dev.os == 'windows' else '/new')
    dev.remove(new_file)
    done = dev.pb('env', 'vault', 'recover', '--password-stdin', '--recovery-input-file', old_file, '--recovery-file', new_file, input=new_password + b'\n', timeout=180)
    assert done.rc == 0, 'recover failed: %s' % (done.text + done.errtext)[-200:]
    fresh = dev.read(new_file)
    for path, data in ((PASSWORD, new_password), (RECOVERY, fresh.strip())):
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        os.write(fd, data)
        os.close(fd)
    dev.remove(stage)
    assert unlock(dev, new_password).rc == 0, 'the recovered password does not unlock'
    return 'recover replaced the password and the recovery code'


def reset_vault(dev, new_password):
    """Reset the vault (destructive for this fresh account's values) so it has `new_password` and a NEW recovery code; the files
    ~/.pbmac/xplat2-vault.pw and xplat2-recovery follow (mode 600, never printed)."""
    account = fleet.json_pb(dev, 'auth', 'status')['account']['id']
    stage = dev.home + ('\\xplat-rec' if dev.os == 'windows' else '/.xplat-rec')
    new_file = stage + ('\\fresh' if dev.os == 'windows' else '/fresh')
    dev.remove(new_file)
    dev.write(stage + ('\\.keep' if dev.os == 'windows' else '/.keep'), b'')
    done = dev.pb('env', 'vault', 'reset', '--confirm', 'RESET ENV ' + account, '--password-stdin', '--recovery-file', new_file,
                  input=new_password + b'\n', timeout=180)
    assert done.rc == 0, 'reset failed: %s' % (done.text + done.errtext)[-200:]
    recovery = dev.read(new_file)
    for path, data in ((PASSWORD, new_password), (RECOVERY, recovery.strip())):
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        os.write(fd, data)
        os.close(fd)
    dev.remove(stage)


def t_lock(dev):
    password = PASSWORD.read_bytes().strip()
    recovery = RECOVERY.read_bytes().strip()
    assert dev.pb('env', 'vault', 'lock').rc == 0, 'lock failed'
    assert locked(dev), 'a locked vault still lists ENV: %s' % dev.pb('env', 'list').text[:120]
    done = unlock(dev, b'wrong-password-' + os.urandom(4).hex().encode())
    assert done.rc != 0, 'a wrong password unlocked the vault'
    assert unlock(dev, password).rc == 0, 'the password did not unlock'
    assert dev.pb('env', 'list').rc == 0, 'ENV not readable after unlock'
    assert dev.pb('env', 'vault', 'lock').rc == 0 and locked(dev)
    # a recovery code is not a password: `unlock` refuses it, `recover` opens the vault and replaces both secrets
    refused = unlock(dev, recovery)
    assert refused.rc != 0, 'unlock accepted a recovery code'
    return_value = recover_and_restore(dev)
    assert dev.pb('env', 'list').rc == 0
    return 'lock refuses ENV, a wrong secret fails, the password unlocks, the recovery code opens it through recover (%s)' % return_value


def t_persist(dev, other):
    """The unlock survives a daemon restart where the OS has a secure store (Keychain on M, Credential Manager on W) and is
    gone on Linux without a Secret Service (keys only in daemon memory, as `pb env vault init --help` says); locking here
    leaves the other device's vault open."""
    password = PASSWORD.read_bytes().strip()
    assert unlock(dev, password).rc == 0 and unlock(other, password).rc == 0
    assert dev.pb('env', 'vault', 'lock').rc == 0 and other.pb('env', 'list').rc == 0, 'locking %s locked %s' % (dev.name, other.name)
    assert unlock(dev, password).rc == 0
    assert dev.pb('service', 'restart', timeout=180).rc == 0, 'service restart failed'
    deadline, readable = time.time() + 90, False
    while time.time() < deadline and not readable:
        time.sleep(5)
        readable = dev.pb('env', 'list').rc == 0
    if dev.os == 'linux':
        assert not readable, 'a Linux machine without a Secret Service kept the unlock across a daemon restart'
        assert unlock(dev, password).rc == 0
        return 'after a daemon restart %s asks for the password again (memory-only custody); locking it left %s open' % (dev.name, other.name)
    assert readable, 'the unlock did not survive a daemon restart on %s (secure store)' % dev.name
    return 'the unlock survived a daemon restart on %s (secure store); locking it left %s open' % (dev.name, other.name)


def t_remove(dev, other):
    """Removing this machine's custody leaves the account's values and the other machine's vault alone."""
    account = fleet.json_pb(dev, 'auth', 'status')['account']['id']
    password = PASSWORD.read_bytes().strip()
    assert unlock(dev, password).rc == 0 and unlock(other, password).rc == 0
    done = dev.pb('env', 'vault', 'remove', '--confirm', 'REMOVE LOCAL ENV ' + account, timeout=120)
    assert done.rc == 0, 'remove failed: %s' % (done.text + done.errtext)[-200:]
    try:
        assert dev.pb('env', 'list').rc != 0, 'ENV is still readable on %s after its custody was removed' % dev.name
        assert other.pb('env', 'list').rc == 0, 'removing custody on %s locked %s' % (dev.name, other.name)
    finally:
        assert unlock(dev, password).rc == 0, 'the vault does not open again on %s after remove' % dev.name
    assert dev.pb('env', 'list').rc == 0
    return 'remove on %s dropped only its own custody; %s kept reading ENV; the password opens it again' % (dev.name, other.name)


def t_leak(dev):
    needles = [env.VALUES['XP_9_LONG'][:200], env.VALUES['XP_8_MULTI'].split('\n')[0] + '\nline two', env.VALUES['XP_5_PERCENT'], env.VALUES['XP_7_EMOJI']]
    if dev.os == 'windows':
        roots = [dev.home + '\\AppData\\Local\\paperboat', dev.home + '\\AppData\\Roaming\\Microsoft\\Windows\\PowerShell\\PSReadLine', 'C:\\ProgramData\\Paperboat']
    elif dev.os == 'darwin':
        roots = [dev.home + '/Library/Application Support/paperboat', dev.home + '/.local/state/paperboat', dev.home + '/.bash_history', dev.home + '/.zsh_history']
    else:
        roots = [dev.home + '/.config/paperboat', dev.home + '/.local/state/paperboat', dev.home + '/.bash_history']
    done = dev.run([py(dev), '-c', SEARCH, os.pathsep.join(roots), *needles], timeout=300)
    hits = [line for line in done.text.splitlines() if line.strip()]
    assert done.rc == 0 and not hits, 'ENV values found in: %s' % hits[:5]
    return 'no ENV value in pb config/diagnostics/logs or shell history on %s (%d locations)' % (dev.name, len(roots))


def t_reset(src, dst):
    """The old password stops working on `dst` once `src` reset the vault; the new one opens it. Then the vault is reset
    again so the account keeps a known password and recovery code."""
    password = PASSWORD.read_bytes().strip()
    new = b'reset-' + os.urandom(12).hex().encode()
    assert unlock(src, password).rc == 0
    reset_vault(src, new)
    try:
        assert unlock(dst, password).rc != 0, 'the old password still opens the vault on %s after a reset on %s' % (dst.name, src.name)
        assert unlock(dst, new).rc == 0, 'the new password does not open the vault on %s' % dst.name
    finally:
        unlock(src, new)
        reset_vault(src, b'xplat-' + os.urandom(12).hex().encode())
        for dev in (src, dst):
            assert unlock(dev, PASSWORD.read_bytes().strip()).rc == 0
    return 'a reset on %s was honoured on %s (old password refused, new accepted); account secrets rotated and kept' % (src.name, dst.name)


ONLY = set(sys.argv[sys.argv.index('--only') + 1].split(',')) if '--only' in sys.argv else set()


def check(name, who, action):
    if ONLY and name not in ONLY:
        return
    key = 'X-13:%s:%s' % (name, who)
    started = time.time()
    try:
        detail = action()
        results[key] = {'status': 'PASS', 'detail': detail}
        print('PASS %s %ds %s' % (key, time.time() - started, detail[:260]), flush=True)
    except Exception as error:  # noqa: BLE001
        results[key] = {'status': 'FAIL', 'detail': str(error)[:600]}
        print('FAIL %s %ds: %s' % (key, time.time() - started, str(error)[:600]), flush=True)


def main():
    argv = sys.argv[1:]
    keys = argv[argv.index('--devices') + 1].split(',') if '--devices' in argv else ['L', 'M', 'W']
    devices = {k: d for k, d in fleet.fleet().items() if k in keys}
    first = next(iter(devices.values()))
    unlock(first, PASSWORD.read_bytes().strip())  # a vault made by an older build may not open: the reset below makes a known one
    reset_vault(first, b'xplat-' + os.urandom(12).hex().encode())  # a vault whose recovery code this run knows
    for dev in devices.values():
        assert unlock(dev, PASSWORD.read_bytes().strip()).rc == 0
    for key, dev in devices.items():
        check('lock', key, lambda: t_lock(dev))
    order = list(devices)
    for i, key in enumerate(order):
        check('remove', key, lambda: t_remove(devices[key], devices[order[(i + 1) % len(order)]]))
    for i, key in enumerate(order):
        check('persist', key, lambda: t_persist(devices[key], devices[order[(i + 1) % len(order)]]))
    for key, dev in devices.items():
        check('leak', key, lambda: t_leak(dev))
    for i, a_key in enumerate(order):
        b_key = order[(i + 1) % len(order)]
        check('reset', '%s>%s' % (a_key, b_key), lambda: t_reset(devices[a_key], devices[b_key]))
    stamp = time.strftime('%Y%m%dT%H%M%SZ', time.gmtime())
    out = Path(__file__).parent / 'results' / ('xplat-vault-%s.json' % stamp)
    out.write_text(json.dumps({'check': 'xplat-vault', 'devices': keys, 'results': [dict(id=k, **v) for k, v in results.items()]}, indent=1))
    failed = [k for k, v in results.items() if v['status'] != 'PASS']
    print('%s %s' % ('FAIL' if failed or not results else 'PASS', out))
    sys.exit(1 if failed or not results else 0)


if __name__ == '__main__':
    main()
