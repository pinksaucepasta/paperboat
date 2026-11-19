# X-02 rename: xplat_rename.py 'L>W' 'W>L' ... (fleet, one account). a has machine.<a> in the shared file; b renames a; a's status
# must reach configuration_changed and `pb config sync apply` must restore healthy. Exit 1 unless every pair does both.
import json, os, re, sys, time
sys.path.insert(0, os.getcwd())
import xplat_fleet as fleet
import xplat_config as c
keys = sorted({k for p in sys.argv[1:] for k in p.split('>')})
devs = {k: d for k, d in fleet.fleet(keys).items()}
for k, d in devs.items(): d.key = k
for k, d in devs.items(): print(k, d.name, d.os, d.pb('version').text.strip()[:60], flush=True)
failed = []
ctx = c.Context(devs)
def status(dev, machine_id):
    return next(e for e in c.pbj(dev, 'config', 'status')['environments'] if e['machine_id'] == machine_id)
def fields(e): return json.dumps({k: e.get(k) for k in ('alias', 'state', 'error_code', 'recovery_actions')})
def one(a_key, b_key):
    a, b = devs[a_key], devs[b_key]
    section = c.shared_with_machine(a.name, c.RULE_ROOT[a.os] + '/settings-section.json')
    c.put(ctx, section)
    mid = fleet.machines()[a.name]['id']
    src = lambda: c.rules(a, c.pbj(a, 'config', 'sync', 'validate'))['cfg/settings.json']['source']
    c.wait('section applies', lambda: src() == 'machine_section', seconds=180, every=8)
    print(a_key + '>' + b_key, 'before rename:', src(), fields(status(a, mid)), flush=True)
    c.apply(a)  # the shared-file change paused a for approval: start from healthy
    c.wait('baseline healthy', lambda: status(a, mid)['state'] == 'healthy', seconds=120, every=6)
    paused = restored = False
    try:
        t_rename = int(time.time())
        assert b.pb('machine', 'rename', mid, a.name + '-r', '--json').rc == 0, 'rename refused'
        print(' rename returned after', int(time.time()) - t_rename, 's', flush=True)
        t0 = time.time()
        while time.time() - t0 < 480:
            e = status(a, mid); print(' ', int(time.time()-t0), fields(e), flush=True)
            if e.get('error_code') == 'configuration_changed': paused = True; break
            time.sleep(8)
        print(' warnings', json.dumps(c.pbj(a, 'config', 'sync', 'validate').get('warnings'))[:200], 'source', src(), flush=True)
        if a.os == 'linux':
            j = a.run(['sh', '-c', "journalctl --user -u paperboatd --since @%d --no-pager | grep -i 'config|alias|descriptor|machine|sync' -E | cut -c1-240 | tail -n 40" % t_rename]).text
            print(' JOURNAL', j, flush=True)
        c.apply(a); t0 = time.time()
        while time.time() - t0 < 180:
            e = status(a, mid); print('  apply', int(time.time()-t0), fields(e), flush=True)
            if e.get('state') == 'healthy': restored = True; break
            time.sleep(8)
    finally:
        b.pb('machine', 'rename', mid, a.name, '--json')
    c.wait('renamed back', lambda: fleet.machines().get(a.name), seconds=60, every=4)
    c.apply(a)
    c.wait('section back', lambda: src() == 'machine_section' and status(a, mid)['state'] == 'healthy', seconds=120, every=8)
    print('RESULT %s>%s paused_configuration_changed=%s apply_restored=%s' % (a_key, b_key, paused, restored), flush=True)
    if not (paused and restored): failed.append(a_key + '>' + b_key)
try:
    ctx.start({'.paperboat/config-sync.jsonc': c.SHARED.encode(), 'cfg/settings.json': b'{}\n'})
    for k in devs: ctx.assign(k)
    for p in sys.argv[1:]:
        try: one(*p.split('>'))
        except Exception as e: print('RESULT', p, 'ERROR', repr(e)[:300], flush=True); failed.append(p)
finally:
    try: c.put(ctx, c.SHARED)
    except Exception as e: print('put', e)
    ctx.finish()
sys.exit(1 if failed else 0)
