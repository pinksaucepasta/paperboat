#!/usr/bin/env python3
"""Hold a real preview while an independent browser verifies its synthetic body."""
import argparse
import functools
import http.server
import json
from pathlib import Path
import signal
import subprocess
import tempfile
import threading
import time
import uuid

from pairing_lifecycle import invoke
from preview_receipt import receipt_value


class QuietHandler(http.server.SimpleHTTPRequestHandler):
    def log_message(self, *args):
        pass


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--mode', choices=['foreground', 'background'], required=True)
    args = parser.parse_args()
    binary = Path.home() / '.local/bin/pb'
    with tempfile.TemporaryDirectory(prefix='paperboat-preview-main-') as directory:
        root = Path(directory)
        marker = 'PB_PREVIEW_' + uuid.uuid4().hex
        (root / 'index.html').write_text('<!doctype html><title>Paperboat smoke</title><p>' + marker + '</p>')
        handler = functools.partial(QuietHandler, directory=directory)
        server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), handler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        process = None
        lease = None
        failure = None
        try:
            arguments = [str(binary), 'preview', str(server.server_port), '--ttl', '5m', '--json']
            if args.mode == 'background':
                arguments.append('--background')
                lease = invoke(binary, *arguments[1:], timeout=60)
            else:
                output = (root / 'foreground.jsonl').open('w+')
                process = subprocess.Popen(arguments, stdout=output, stderr=subprocess.DEVNULL)
                deadline = time.monotonic() + 60
                while lease is None:
                    output.seek(0)
                    for line in output:
                        try:
                            value = json.loads(line)
                            receipt_value(value, 'endpoint')
                            lease = value
                        except (ValueError, TypeError):
                            pass
                    if process.poll() is not None or time.monotonic() >= deadline:
                        raise RuntimeError('foreground preview did not publish a ready receipt')
                    if lease is None:
                        time.sleep(.1)
            url = receipt_value(lease, 'endpoint')
            identity = receipt_value(lease, 'id')
            gate = root / 'browser-verified'
            failed = root / 'browser-failed'
            print(json.dumps({'state': 'browser_required', 'mode': args.mode, 'url': url,
                              'marker': marker, 'gate': str(gate), 'failure_gate': str(failed),
                              'id': identity}), flush=True)
            deadline = time.monotonic() + 180
            while not gate.exists():
                if failed.exists():
                    raise RuntimeError('independent browser could not load the preview')
                if time.monotonic() >= deadline:
                    raise RuntimeError('independent browser did not verify preview within180s')
                time.sleep(.1)
            print(json.dumps({'check': 'preview-' + args.mode, 'status': 'PASS',
                              'independent_browser_verified': True}), flush=True)
        except Exception as error:
            failure = error
            raise
        finally:
            if process is not None:
                if process.poll() is None:
                    process.send_signal(signal.SIGINT)
                try:
                    process.wait(timeout=15)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()
                output.close()
            try:
                if lease is not None:
                    invoke(binary, 'preview', 'stop', lease['id'], '--json', timeout=30)
            except Exception as error:
                if failure is None:
                    raise
                failure.add_note('Preview cleanup failed: ' + str(error))
            finally:
                server.shutdown()
                server.server_close()
                thread.join(timeout=5)


if __name__ == '__main__':
    main()
