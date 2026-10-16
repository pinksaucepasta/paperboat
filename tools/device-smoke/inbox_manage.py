#!/usr/bin/env python3
"""Verify real Inbox delivery after path changes, restoring both machines' settings."""
import argparse
import hashlib
import json
from pathlib import Path
import tempfile
import uuid

from install_lifecycle import registration
from pairing_lifecycle import invoke
from ssh_tools_lifecycle import remote


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('target')
    args = parser.parse_args()
    binary = Path.home() / '.local/bin/pb'
    before = registration()
    name = 'smoke-inbox-' + uuid.uuid4().hex
    payload = b'exact configured Inbox fixture\n'
    files = [name + '-custom.txt', name + '-reset.txt']

    def receiver(*arguments):
        code = ("from pathlib import Path\nimport json,subprocess,sys\n"
                "r=subprocess.run([str(Path.home()/'.local/bin/pb'),'inbox',*"
                + repr(list(arguments)) + ",'--json'],stdin=subprocess.DEVNULL,capture_output=True,timeout=30)\n"
                "sys.stdout.buffer.write(r.stdout)\nsys.stderr.buffer.write(r.stderr)\nraise SystemExit(r.returncode)\n")
        return json.loads(remote(binary, args.target, code))['data']

    original = receiver('path')['path']
    policy = invoke(binary, 'inbox', 'policy', '--json')
    setup = ("from pathlib import Path\nimport json,tempfile\n"
             "root=Path.home()/'Paperboat'\nroot.mkdir(exist_ok=True)\n"
             "print(json.dumps({'custom':tempfile.mkdtemp(prefix=" + repr(name)
             + ",dir=root),'default':str(root/'Inbox')}))\n")
    paths = json.loads(remote(binary, args.target, setup))
    failure = None
    touched_policy = False

    def send(root, filename, source):
        batch = invoke(binary, 'send', str(source), '--to', args.target, '--json', timeout=60)
        if len(batch['transfers']) != 1 or batch['transfers'][0]['sha256'] != hashlib.sha256(payload).hexdigest():
            raise RuntimeError('Inbox delivery lost its exact published manifest')
        check = ("from pathlib import Path\nimport hashlib\np=Path(" + repr(root) + ")/" + repr(filename)
                 + "\nassert p.exists(),'Inbox setting did not change the actual receiving directory'\n"
                 "assert hashlib.sha256(p.read_bytes()).hexdigest()==" + repr(hashlib.sha256(payload).hexdigest()) + "\n")
        remote(binary, args.target, check)

    with tempfile.TemporaryDirectory(prefix=name) as directory:
        sources = [Path(directory) / filename for filename in files]
        for source in sources:
            source.write_bytes(payload)
        try:
            changed = receiver('set', paths['custom'])
            if changed['path'] != paths['custom'] or receiver('path')['path'] != paths['custom']:
                raise RuntimeError('Inbox set did not retain its exact path')
            send(paths['custom'], files[0], sources[0])
            reset = receiver('reset')
            if reset['path'] != paths['default']:
                raise RuntimeError('Inbox reset did not select its documented default')
            send(paths['default'], files[1], sources[1])
            if not isinstance(invoke(binary, 'inbox', 'requests', '--json')['requests'], list):
                raise RuntimeError('Inbox requests did not return its collection')
            touched_policy = True
            for mode in ['manual', 'automatic']:
                selected = invoke(binary, 'inbox', 'policy', mode, '--json')
                observed = invoke(binary, 'inbox', 'policy', '--json')
                if selected['acceptance'] != mode or observed['acceptance'] != mode:
                    raise RuntimeError('Inbox policy did not retain its selected acceptance')
        except Exception as error:
            failure = error
            raise
        finally:
            try:
                if receiver('set', original)['path'] != original:
                    raise RuntimeError('Inbox original path was not restored')
                if touched_policy:
                    options = ['--receipt-email'] if policy['receipt_email'] else []
                    invoke(binary, 'inbox', 'policy', policy['acceptance'], *options, '--json')
                cleanup = ("from pathlib import Path\nimport shutil\nroots="
                           + repr([original, paths['custom'], paths['default']]) + "\n"
                           "for root in roots:\n for filename in " + repr(files)
                           + ":\n  assert filename.startswith(" + repr(name) + ")\n  (Path(root)/filename).unlink(missing_ok=True)\n"
                           "custom=Path(" + repr(paths['custom']) + ")\nassert custom.parent==Path.home()/'Paperboat' and custom.name.startswith("
                           + repr(name) + ")\nshutil.rmtree(custom)\n")
                remote(binary, args.target, cleanup)
            except Exception as cleanup_error:
                if failure is None:
                    raise
                failure.add_note('Exact Inbox restoration failed: ' + str(cleanup_error))
    if registration() != before:
        raise RuntimeError('Inbox management changed the sender enrollment')
    print(json.dumps({'check': 'inbox-manage', 'target': args.target, 'status': 'PASS',
                      'phases': ['path', 'set', 'reset', 'actual_delivery', 'requests', 'policy'],
                      'settings_restored': True, 'enrollment_unchanged': True}))


if __name__ == '__main__':
    main()
