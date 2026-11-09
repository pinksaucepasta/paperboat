#!/usr/bin/env python3
"""Hold a real preview while an independent browser verifies its synthetic body."""
import argparse
import contextlib
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
import winhost
from preview_receipt import PREVIEW_COMMAND_TIMEOUT, receipt_value


class QuietHandler(http.server.SimpleHTTPRequestHandler):
    def log_message(self, *args):
        pass


@contextlib.contextmanager
def serving(body):
    """A static origin answering every request with body, on the device under test; yields its loopback port."""
    device = winhost.device()
    if device:  # pb dials the device's own loopback, so the origin runs there (a PowerShell listener)
        with device.origin(body) as port:
            yield port
        return
    with tempfile.TemporaryDirectory(prefix='paperboat-origin-') as directory:
        (Path(directory) / 'index.html').write_text(body)
        server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), functools.partial(QuietHandler, directory=directory))
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            yield server.server_port
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=5)


@contextlib.contextmanager
def exposed(local_port):
    """The port pb must be given for a server this check runs itself: unchanged on Unix, an ssh -R forward on Windows."""
    device = winhost.device()
    if not device:
        yield local_port
        return
    with device.forward(local_port) as port:
        yield port


def start_foreground(binary, arguments, root):
    """Run `pb preview ... --json` in the foreground and return (first receipt with an endpoint, stop function).

    stop() sends SIGINT on Unix; Windows has no console signal to a hidden process, so its process tree is killed
    and the caller's `pb preview stop` removes the lease."""
    device = winhost.device()
    if device:
        scratch = device.temp_dir('pb-preview-foreground')
        directory = scratch.__enter__()
        process = device.spawn([str(binary), *arguments], directory)

        def lines():
            return process.stdout().decode(errors='replace').splitlines()

        def finished():
            return process.returncode() is not None

        def stop():
            process.kill()
            scratch.__exit__(None, None, None)
    else:
        output = (root / 'foreground.jsonl').open('w+')
        process = subprocess.Popen([str(binary), *arguments], stdout=output, stderr=subprocess.DEVNULL)

        def lines():
            output.seek(0)
            return list(output)

        def finished():
            return process.poll() is not None

        def stop():
            if process.poll() is None:
                process.send_signal(signal.SIGINT)
            try:
                process.wait(timeout=15)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
            output.close()
    deadline = time.monotonic() + PREVIEW_COMMAND_TIMEOUT
    while True:
        for line in lines():
            try:
                value = json.loads(line)
                receipt_value(value, 'endpoint')
                return value, stop
            except (ValueError, TypeError):
                pass
        if finished() or time.monotonic() >= deadline:
            stop()
            raise RuntimeError('foreground preview did not publish a ready receipt')
        time.sleep(.1)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--mode', choices=['foreground', 'background'], required=True)
    args = parser.parse_args()
    binary = winhost.pb_path()
    marker = 'PB_PREVIEW_' + uuid.uuid4().hex
    page = '<!doctype html><title>Paperboat smoke</title><p>' + marker + '</p>'
    with tempfile.TemporaryDirectory(prefix='paperboat-preview-main-') as directory, serving(page) as port:
        root = Path(directory)
        stop = None
        lease = None
        failure = None
        try:
            arguments = ['preview', str(port), '--ttl', '5m', '--json']
            if args.mode == 'background':
                arguments.append('--background')
                lease = invoke(binary, *arguments, timeout=PREVIEW_COMMAND_TIMEOUT)
            else:
                lease, stop = start_foreground(binary, arguments, root)
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
            if stop is not None:
                stop()
            try:
                if lease is not None:
                    invoke(binary, 'preview', 'stop', lease['id'], '--json', timeout=30)
            except Exception as error:
                if failure is None:
                    raise
                failure.add_note('Preview cleanup failed: ' + str(error))


if __name__ == '__main__':
    main()
