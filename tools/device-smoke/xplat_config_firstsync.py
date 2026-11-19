#!/usr/bin/env python3
"""Config sync: files written on a machine while its first sync after an assignment runs are all published.

  xplat_config_firstsync.py [--devices L,W]   (the first device, a Linux one, writes; the second receives)

The writer creates a file every 50 ms from before the assignment is applied until well after the first sync, so some
land between the sync's snapshot and its commit. Every one must reach the repository and the other machine without
being edited again. Runs under the same fixture as xplat_config.py (repository on the repository host, one assignment per device).
"""
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
import xplat_config as c  # noqa: E402
import xplat_fleet as fleet  # noqa: E402

COUNT = 400


def main():
    argv = sys.argv[1:]
    keys = argv[argv.index('--devices') + 1].split(',') if '--devices' in argv else ['L', 'W']
    devices = fleet.fleet(keys)
    writer, reader = devices[keys[0]], devices[keys[1]]
    ctx = c.Context(devices)
    try:
        ctx.start({'.paperboat/config-sync.jsonc': c.SHARED.encode(), 'cfg/settings.json': b'{}\n'})
        folder = c.real(writer, 'app', 'firstsync')
        loop = ('mkdir -p %s; i=0; while [ $i -lt %d ]; do printf "file $i\\n" > %s/f$i.txt.part && mv %s/f$i.txt.part %s/f$i.txt; '
                'i=$((i+1)); sleep 0.05; done' % (folder, COUNT, folder, folder, folder))
        writer.sh('(setsid nohup bash -c %r > /dev/null 2>&1 < /dev/null &)' % loop)
        for key in keys:
            ctx.assign(key)
        listing = 'git --git-dir %s/pair.git ls-tree -r --name-only main cfg/app/firstsync | grep -c "\\.txt$"' % c.REPO_ROOT
        count = lambda: int(ctx.repo.sh(listing).stdout.decode().strip() or 0)  # noqa: E731
        end = time.time() + 120
        while count() < COUNT and time.time() < end:
            time.sleep(10)
        published = count()
        if published < COUNT:
            names = ctx.repo.sh('git --git-dir %s/pair.git ls-tree -r --name-only main cfg/app/firstsync' % c.REPO_ROOT).stdout.decode().split()
            have = {int(n.split('/f')[-1][:-4]) for n in names if n.endswith('.txt')}
            raise AssertionError('%d of %d files published; missing %s' % (published, COUNT, sorted(set(range(COUNT)) - have)[:20]))
        sample = sorted({0, COUNT - 1, *range(0, COUNT, 20)})
        missing = list(sample)
        end = time.time() + 300
        while missing and time.time() < end:
            missing = [i for i in missing if reader.read(c.real(reader, 'app', 'firstsync', 'f%d.txt' % i)) != b'file %d\n' % i]
            if missing:
                time.sleep(10)
        assert not missing, '%s did not receive %s' % (reader.name, missing[:20])
        got = len(sample)
        print('PASS %d of %d files written during the first sync were published and reached %s (%d sampled)' % (published, COUNT, reader.name, got))
    finally:
        ctx.finish()


if __name__ == '__main__':
    main()
