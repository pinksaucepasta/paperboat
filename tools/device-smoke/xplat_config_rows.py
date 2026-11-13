"""X-04, X-05, X-06 (collisions) and X-07 (names): checks for xplat_config.py. Each takes (ctx, a, b, direction): a is the device
that acts, b the peer; run_pairs calls it for every ordered pair, so both directions of a pair are covered."""
import shlex
import unicodedata

import xplat_config as x
from xplat_config import pbj, real, wait

APP = 'x-app'  # below the directory rule's repository path cfg/app/


def unix(dev):
    return dev.os != 'windows'


def mode_of(dev, path):
    """Permission bits as octal text (Unix devices)."""
    q = shlex.quote(path)
    return dev.sh('stat -c %%a %s 2>/dev/null || stat -f %%Lp %s' % (q, q)).text.strip()


def is_executable(dev, path):
    return dev.run(['test', '-x', path]).rc == 0


def status_of(dev):
    for env in pbj(dev, 'config', 'status').get('environments', []):
        if env.get('alias') == dev.name:
            return env
    raise AssertionError('no status for ' + dev.name)


def skipped(dev):
    return {s['path']: s['reason'] for s in status_of(dev).get('skipped') or []}


def wait_skipped(dev, path, reason, seconds=150):
    wait('%s reports %s as %s' % (dev.name, path, reason), lambda: skipped(dev).get(path) == reason, seconds, every=6)


def repo_mode(ctx, path):
    out = ctx.repo.sh('git --git-dir %s/pair.git ls-tree main %s' % (x.REPO_ROOT, shlex.quote(path))).stdout.decode()
    return out.split()[0] if out else None


def app_path(dev, *parts):
    return x.real(dev, 'app', *parts)


def x04_exec(ctx, a, b, direction):
    """X-04: the executable intent travels between Unix devices and through Windows and back; a file authored on W is not executable."""
    name = 'exec-%s.txt' % direction.replace('>', '')
    repo = 'cfg/app/%s/%s' % (APP, name)
    data = b'#!/bin/sh\necho x04\n'
    path_a, path_b = app_path(a, APP, name), app_path(b, APP, name)
    a.write(path_a, data, mode='755' if unix(a) else None)
    wait('%s has the file' % b.name, lambda: b.read(path_b) == data)
    expected = '100755' if unix(a) else '100644'
    assert repo_mode(ctx, repo) == expected, 'repository mode %s, want %s' % (repo_mode(ctx, repo), expected)
    if unix(b):
        assert is_executable(b, path_b) == unix(a), '%s: executable=%s after arriving from %s' % (b.name, is_executable(b, path_b), a.name)
    detail = 'repository %s, %s executable=%s' % (expected, b.name, unix(a) if unix(b) else 'n/a')
    if unix(a) and not unix(b):  # back through Windows: its edit keeps the executable intent for the Unix side
        edited = data + b'echo edited on windows\n'
        b.write(path_b, edited)
        wait('%s received the Windows edit' % a.name, lambda: a.read(path_a) == edited)
        assert repo_mode(ctx, repo) == '100755' and is_executable(a, path_a), 'the executable intent was lost through Windows'
        detail += '; kept through a Windows edit and back'
    return detail


def x04_local_bits(ctx, a, b, direction):
    """X-04: the local permission bits of an existing destination are kept when its content is replaced."""
    if not unix(b):
        return 'n/a: %s has no permission bits (the executable intent is covered by x04_exec)' % b.name
    name = 'bits-%s.txt' % direction.replace('>', '')
    path_a, path_b = app_path(a, APP, name), app_path(b, APP, name)
    a.write(path_a, b'v1\n')
    wait('%s has v1' % b.name, lambda: b.read(path_b) == b'v1\n')
    assert b.sh('chmod 640 %s' % shlex.quote(path_b)).rc == 0
    a.write(path_a, b'v2\n')
    wait('%s has v2' % b.name, lambda: b.read(path_b) == b'v2\n')
    assert mode_of(b, path_b) == '640', '%s: mode %s after the replacement, want 640' % (b.name, mode_of(b, path_b))
    return 'mode 640 kept on %s' % b.name


def make_unsafe(dev, path):
    """Make the file one the device refuses to sync: world-writable on Unix, owned by another principal on Windows."""
    if unix(dev):
        assert dev.sh('chmod 666 %s' % shlex.quote(path)).rc == 0
    else:
        done = dev.sh("icacls '%s' /setowner 'NT AUTHORITY\\LOCAL SERVICE' | Out-Null; exit $LASTEXITCODE" % path)
        assert done.rc == 0, done.errtext[-200:]


def make_safe(dev, path):
    if unix(dev):
        dev.sh('chmod 644 %s' % shlex.quote(path))
    else:
        # whoami, not %USERDOMAIN%\%USERNAME%: the domain variable is WORKGROUP on a runner and icacls fails with 1332
        done = dev.sh("icacls '%s' /setowner (whoami) | Out-Null; exit $LASTEXITCODE" % path)
        assert done.rc == 0, done.errtext[-200:]


def x04_unsafe(ctx, a, b, direction):
    """X-04: an unsafe file (world-writable on Unix, owned by someone else on Windows) is skipped with unsafe_permissions; its siblings sync."""
    tag = direction.replace('>', '')
    bad, good = 'unsafe-%s.txt' % tag, 'sibling-%s.txt' % tag
    repo_bad = 'cfg/app/%s/%s' % (APP, bad)
    path_bad = app_path(a, APP, bad)
    a.write(path_bad, b'unsafe\n')
    make_unsafe(a, path_bad)
    a.write(app_path(a, APP, good), b'sibling\n')
    wait('the sibling reaches %s' % b.name, lambda: b.read(app_path(b, APP, good)) == b'sibling\n')
    wait_skipped(a, 'cfg/app/%s/%s' % (APP, bad), 'unsafe_permissions')
    assert ctx.repo.show(repo_bad) is None and not b.exists(app_path(b, APP, bad)), 'an unsafe file travelled'
    assert status_of(a)['state'] in ('warning', 'healthy'), status_of(a)['state']
    make_safe(a, path_bad)
    wait('the file travels once it is safe', lambda: b.read(app_path(b, APP, bad)) == b'unsafe\n')
    return 'skipped as unsafe_permissions, sibling synced, travelled once safe'


def link(dev, target, path):
    """Create a link at path (symlink on Unix; a symlink, plus a junction for directories, on Windows)."""
    if unix(dev):
        assert dev.sh('ln -s %s %s' % (shlex.quote(target), shlex.quote(path))).rc == 0
    else:
        done = dev.sh("New-Item -ItemType SymbolicLink -Path '%s' -Target '%s' | Out-Null" % (path, target))
        assert done.rc == 0, done.errtext[-300:]


def junction(dev, target, path):
    done = dev.sh("New-Item -ItemType Junction -Path '%s' -Target '%s' | Out-Null" % (path, target))
    assert done.rc == 0, done.errtext[-300:]


def x05_local_links(ctx, a, b, direction):
    """X-05: a link in a mapped directory (symlink, and on W a junction) is reported unsupported_file_type, never followed or synced."""
    tag = direction.replace('>', '')
    target = a.home + sep_of(a) + 'x05-target-' + tag
    a.write(target + sep_of(a) + 'inside.txt', b'inside the target\n')
    a.write(target + sep_of(a) + 'file.txt', b'target file\n')
    checks = {'filelink-%s.txt' % tag: lambda p: link(a, target + sep_of(a) + 'file.txt', p)}
    if unix(a):
        checks['dirlink-%s' % tag] = lambda p: link(a, target, p)
    else:
        checks['junction-%s' % tag] = lambda p: junction(a, target, p)
    a.write(app_path(a, APP, 'x05-ok-%s.txt' % tag), b'ok\n')
    for name, make in checks.items():
        make(app_path(a, APP, name))
    wait('the sibling reaches %s' % b.name, lambda: b.read(app_path(b, APP, 'x05-ok-%s.txt' % tag)) == b'ok\n')
    for name in checks:
        wait_skipped(a, 'cfg/app/%s/%s' % (APP, name), 'unsupported_file_type')
        assert ctx.repo.show('cfg/app/%s/%s' % (APP, name)) is None and not b.exists(app_path(b, APP, name)), name + ' travelled'
    assert not b.exists(app_path(b, APP, 'dirlink-%s' % tag, 'inside.txt')) and not b.exists(app_path(b, APP, 'junction-%s' % tag, 'inside.txt'))
    assert a.read(target + sep_of(a) + 'inside.txt') == b'inside the target\n' and a.read(target + sep_of(a) + 'file.txt') == b'target file\n', 'a link target changed'
    return 'links reported unsupported_file_type: %s; targets untouched' % ', '.join(sorted(checks))


def sep_of(dev):
    return x.sep(dev)


def x05_repo_link(ctx, a, b, direction):
    """X-05: a repository file that is a symlink is never materialised on a device (reported, not created, nothing outside written)."""
    tag = direction.replace('>', '')
    name = 'repolink-%s.txt' % tag
    outside = b.home + sep_of(b) + 'x05-outside-' + tag + '.txt'
    script = ('work=$(mktemp -d) && git clone -q %s/pair.git $work && cd $work && mkdir -p cfg/app/%s && ln -s /tmp/%s-target cfg/app/%s/%s '
              '&& git add -A && %s git commit -q -m link && git push -q origin HEAD:main && rm -rf $work'
              % (x.REPO_ROOT, APP, tag, APP, name, x.GIT_ENV))
    done = ctx.repo.sh(script)
    assert done.returncode == 0, 'could not commit a symlink: ' + done.stderr.decode()[-200:]
    try:
        for dev in (a, b):
            wait_skipped(dev, 'cfg/app/%s/%s' % (APP, name), 'unsupported_file_type')
            assert not dev.exists(app_path(dev, APP, name)), '%s materialised the repository symlink' % dev.name
        assert not b.exists(outside)
    finally:
        ctx.repo.commit({'cfg/app/%s/%s' % (APP, name): None})
    return 'repository symlink reported on both devices, not created'


def x05_ancestor(ctx, a, b, direction):
    """X-05: a link in an ancestor of a destination is refused at validation; the link target stays untouched."""
    tag = direction.replace('>', '')
    for dev in (a, b):
        target = dev.home + sep_of(dev) + 'x05-anc-target-' + tag
        linkdir = dev.home + sep_of(dev) + 'x05-anc-link-' + tag
        dev.write(target + sep_of(dev) + 'keep.txt', b'keep\n')
        if unix(dev):
            link(dev, target, linkdir)
            rule = '~/x05-anc-link-%s/inner/f.txt' % tag
        else:
            junction(dev, target, linkdir)
            rule = '%%USERPROFILE%%/x05-anc-link-%s/inner/f.txt' % tag
        try:
            x.refuses(dev, ctx, x.rule('cfg/x05-anc.txt', rule), 'a destination behind a link')
            assert dev.read(target + sep_of(dev) + 'keep.txt') == b'keep\n' and not dev.exists(target + sep_of(dev) + 'inner')
        finally:
            dev.remove(linkdir)
            dev.remove(target)
    return 'a destination behind a link is refused on %s and %s; target untouched' % (a.name, b.name)


def x06_case_collision(ctx, a, b, direction):
    """X-06: A.txt and a.txt in a directory rule are reported as a case collision and synced to neither side."""
    tag = direction.replace('>', '')
    upper, lower = 'Case-%s.txt' % tag, 'case-%s.txt' % tag
    ctx.repo.commit({'cfg/app/%s/%s' % (APP, upper): b'upper\n', 'cfg/app/%s/%s' % (APP, lower): b'lower\n'})
    try:
        for dev in (a, b):
            for name in (upper, lower):
                wait_skipped(dev, 'cfg/app/%s/%s' % (APP, name), 'case_collision')
            assert not dev.exists(app_path(dev, APP, upper)) and not dev.exists(app_path(dev, APP, lower)), dev.name + ' created one of them'
    finally:
        ctx.repo.commit({'cfg/app/%s/%s' % (APP, upper): None, 'cfg/app/%s/%s' % (APP, lower): None})
    return 'both spellings reported as case_collision on %s and %s, created on neither' % (a.name, b.name)


def x06_windows_case(ctx, a, b, direction):
    """X-06: on Windows two absolute destinations that differ only by case are one path (the rules overlap)."""
    for dev in (a, b):
        if dev.os != 'windows':
            continue
        upper = (dev.home + '\\x06w\\App.json').replace('\\', '\\\\')
        lower = (dev.home.swapcase() + '\\x06w\\APP.json').replace('\\', '\\\\')
        x.refuses(dev, ctx, x.rule('cfg/x06w-1.txt', upper) + x.rule('cfg/x06w-2.txt', lower), 'two destinations that differ only by case')
        return 'refused on ' + dev.name
    return 'n/a: no Windows device in this pair'


def listing(dev, directory):
    """Names in a directory as decoded text."""
    if unix(dev):
        out = dev.sh("python3 -c \"import os,sys;print('\\n'.join(sorted(os.listdir(sys.argv[1]))))\" %s" % shlex.quote(directory)).out
    else:
        out = dev.sh("[Console]::OutputEncoding=[Text.UTF8Encoding]::new(); (Get-ChildItem -LiteralPath '%s' -Name) -join \"`n\"" % directory).out
    return [n for n in out.decode('utf-8', 'replace').replace('\r', '').split('\n') if n]


def x07_unicode(ctx, a, b, direction):
    """X-07: Unicode names round-trip; a name created decomposed on one device never becomes a second file or a conflict."""
    tag = direction.replace('>', '')
    nfc, nfd = unicodedata.normalize('NFC', 'caf\u00e9-%s.txt' % tag), unicodedata.normalize('NFD', 'caf\u00e9-%s.txt' % tag)
    other = 'z\u00fcrich-\u65e5\u672c\u8a9e-%s.txt' % tag
    for name, data in ((nfd, b'decomposed\n'), (other, b'cjk\n')):
        a.write(app_path(a, APP, name), data)
    wait('%s has both names' % b.name, lambda: sum(1 for n in listing(b, app_path(b, APP)) if unicodedata.normalize('NFC', n) in
                                                   (nfc, unicodedata.normalize('NFC', other))) == 2)
    edited = b'edited on the peer\n'
    names = [n for n in listing(b, app_path(b, APP)) if unicodedata.normalize('NFC', n) == nfc]
    assert len(names) == 1, '%s holds %d files for one name: %r' % (b.name, len(names), names)
    b.write(app_path(b, APP, names[0]), edited)
    wait('%s sees the edit' % a.name, lambda: any(a.read(app_path(a, APP, n)) == edited for n in listing(a, app_path(a, APP))
                                                  if unicodedata.normalize('NFC', n) == nfc))
    for dev in (a, b):
        same = [n for n in listing(dev, app_path(dev, APP)) if unicodedata.normalize('NFC', n) == nfc]
        assert len(same) == 1, '%s holds %d files for one name: %r' % (dev.name, len(same), same)
        assert not status_of(dev).get('conflicts'), '%s: a conflict for a renormalised name' % dev.name
    return 'NFD/NFC name is one file on both devices, edit round-trips, CJK name intact'


def x07_long_path(ctx, a, b, direction):
    """X-07: a destination over 260 characters on Windows works (the file arrives byte-identical)."""
    if 'windows' not in (a.os, b.os):
        return 'n/a: no Windows device in this pair'
    tag = direction.replace('>', '')
    parts = [APP] + ['long-directory-name-%02d-%s' % (i, 'x' * 30) for i in range(6)] + ['deep-%s.txt' % tag]
    data = b'deep file\n'
    w, other = (a, b) if a.os == 'windows' else (b, a)
    other.write(app_path(other, *parts), data)
    long_w = app_path(w, *parts)
    assert len(long_w) > 260, len(long_w)
    wait('the long path reaches %s' % w.name, lambda: w.read('\\\\?\\' + long_w) == data)
    return 'a %d-character destination path arrived byte-identical on %s' % (len(long_w), w.name)


ROW_CHECKS = [('X-04', x04_exec), ('X-04', x04_local_bits), ('X-04', x04_unsafe), ('X-05', x05_local_links), ('X-05', x05_repo_link),
              ('X-05', x05_ancestor), ('X-06', x06_case_collision), ('X-06', x06_windows_case),
              ('X-07', x07_unicode), ('X-07', x07_long_path)]
