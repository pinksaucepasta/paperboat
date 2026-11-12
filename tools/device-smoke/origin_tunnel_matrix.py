#!/usr/bin/env python3
"""Origin matrix through a REAL tunnel: Caddy (certs, tls internal), Traefik, nginx and Apache set up to force HTTPS,
the app alone, and a gRPC server, each reached from the internet through the production edge.

The machine under test is a systemd container (see tools/device-smoke/README.md, `mk.sh` on the container host) paired
to the test account. Every origin is a Docker container on the same host that shares the machine container's network
namespace (`--network container:<ct>`), so the agent reaches them on 127.0.0.1 exactly as it reaches a local app.
One tunnel per origin; its default route is moved between the plain-http, https and (for gRPC) h2c origins and the
public https:// address is probed from here:

  origin_tunnel_matrix.py --host pbvm4 --container small-t1 --out results/origin-tunnel-matrix-YYYYMMDD.json

Probes: C1 forwarded headers (spoofed X-Forwarded-* dropped, proto/host are the edge's), C2 an http origin that redirects
to https shows the redirect-loop page, F an app that forces https works, C8 SSE arrives unbuffered, C12 Range, C13
Expect: 100-continue with a 5 MiB upload, C11 a slow first byte, and C9 gRPC (unary Check and a streaming Watch,
grpc-status trailers). Prints one table and writes every cell as JSON. Tunnels and containers carry the prefix small-
and are removed at the end (--keep leaves them).
"""
import argparse
import json
import os
import shlex
import ssl
import subprocess
import sys
import time
import http.client

import winhost

HERE = os.path.dirname(os.path.abspath(__file__))
MATRIX = os.path.join(HERE, 'origin_proxy_matrix')
GRPC_SERVER = r'''
import time, grpc
from concurrent import futures
from grpc_health.v1 import health, health_pb2, health_pb2_grpc
server = grpc.server(futures.ThreadPoolExecutor(max_workers=8))
servicer = health.HealthServicer()
servicer.set("", health_pb2.HealthCheckResponse.SERVING)
health_pb2_grpc.add_HealthServicer_to_server(servicer, server)
server.add_insecure_port("0.0.0.0:50051")
server.start()
time.sleep(1)
while True:
    time.sleep(3)
    servicer.set("", health_pb2.HealthCheckResponse.NOT_SERVING)
    time.sleep(3)
    servicer.set("", health_pb2.HealthCheckResponse.SERVING)
'''
GRPC_CLIENT = r'''
import json, sys, grpc
from grpc_health.v1 import health_pb2, health_pb2_grpc
host = sys.argv[1]
channel = grpc.secure_channel(host + ":443", grpc.ssl_channel_credentials())
stub = health_pb2_grpc.HealthStub(channel)
out = {}
try:
    reply = stub.Check(health_pb2.HealthCheckRequest(service=""), timeout=20)
    out["unary"] = health_pb2.HealthCheckResponse.ServingStatus.Name(reply.status)
    seen = []
    for update in stub.Watch(health_pb2.HealthCheckRequest(service=""), timeout=20):
        seen.append(health_pb2.HealthCheckResponse.ServingStatus.Name(update.status))
        if len(seen) >= 2 and seen[-1] != seen[0]:
            break
    out["stream"] = seen
except grpc.RpcError as error:
    out["error"] = "%s %s" % (error.code(), (error.details() or "")[:200])
print(json.dumps(out))
'''


class Lab:
    def __init__(self, args):
        self.args = args
        self.cells = []
        self.home = self.on_host('echo $HOME').stdout.strip() if not args.windows else ''
        self.tunnels = []
        self.containers = []
        self.previews = []
        self.moved_host = None
        self.suffix = time.strftime('%d%H%M')  # a deleted tunnel keeps its name, so every run names its own

    # --- remote plumbing -------------------------------------------------------------------------------------
    def on_host(self, script, timeout=300, check=True):
        done = subprocess.run(['ssh', '-o', 'BatchMode=yes', self.args.host, script], capture_output=True, text=True, timeout=timeout)
        if check and done.returncode:
            raise RuntimeError('host command failed (%s): %s' % (done.returncode, (done.stderr or done.stdout)[-300:]))
        return done

    def docker(self, *argv, timeout=300, check=True):
        return self.on_host('sudo docker ' + shlex.join(argv), timeout, check)

    def inside(self, *argv, timeout=300, check=True):
        prefix = ('sudo docker exec -u pbt -e HOME=/home/pbt -e XDG_RUNTIME_DIR=/run/user/1000 -w /home/pbt %s ' % self.args.container)
        return self.on_host(prefix + shlex.join(argv), timeout, check)

    def pb(self, *argv, timeout=300):
        if self.args.windows:  # pb.exe on the Windows device (PB_SMOKE_WINDOWS_PB and SMOKE_WINDOWS_SSH select it)
            done = winhost.pb_path().run_remote([*argv, '--json'], timeout=timeout)
        else:
            done = self.inside(self.args.pb, *argv, '--json', timeout=timeout, check=False)
        try:
            value = json.loads(done.stdout)
        except ValueError:
            raise RuntimeError('pb %s: no JSON (%s)' % (' '.join(argv[:3]), (done.stderr or done.stdout)[-300:]))
        if done.returncode and not (isinstance(value, dict) and value.get('ok') is not False and 'error' not in value):
            raise RuntimeError('pb %s failed: %s' % (' '.join(argv[:3]), json.dumps(value.get('error', value))[:300]))
        return value.get('data', value) if isinstance(value, dict) and value.get('ok') else value

    # --- cells ---------------------------------------------------------------------------------------------
    def cell(self, server, mode, result, detail=''):
        record = {'server': server, 'mode': mode, 'result': result, 'detail': str(detail)[:300]}
        self.cells.append(record)
        print(json.dumps(record), flush=True)

    # --- public probes ----------------------------------------------------------------------------------
    def request(self, host, method, path, headers=None, body=None, timeout=60):
        connection = http.client.HTTPSConnection(host, 443, timeout=timeout, context=ssl.create_default_context())
        started = time.time()
        connection.request(method, path, body=body, headers=headers or {})
        response = connection.getresponse()
        return connection, response, started

    def probe_all(self, server, mode, host, expect_loop=False, deep=True):
        spoof = {'X-Forwarded-For': '203.0.113.9', 'X-Forwarded-Host': 'evil.example.test', 'X-Forwarded-Proto': 'http', 'Forwarded': 'for=203.0.113.9;host=evil.example.test'}
        tries = 0
        try:
            for tries in range(1, 4):
                conn, response, _ = self.request(host, 'GET', '/', spoof)
                body = response.read(8192).decode('utf-8', 'replace')
                error = response.getheader('X-Paperboat-Error', '')
                conn.close()
                # The first request after a route moved can reach the edge before every node knows the new origin.
                if not (response.status == 502 and error == 'upstream_unavailable'):
                    break
                time.sleep(4)
        except Exception as exc:
            self.cell(server, mode, 'visitor_error', exc)
            return
        if tries > 1:
            self.cell(server, mode + '/first-request', 'retried', 'the first request answered upstream_unavailable; %d tries to a stable answer' % tries)
        if expect_loop:
            self.cell(server, mode + '/C2-loop', 'ok' if response.status == 502 and error == 'origin_redirect_loop' else 'unexpected',
                      'status %s error %r' % (response.status, error))
            return
        try:
            seen = json.loads(body)
        except ValueError:
            self.cell(server, mode + '/C1-forwarded', 'unexpected', 'status %s error %r %.120r' % (response.status, error, body))
            return
        xff, xfh = str(seen.get('xff')), str(seen.get('xfh'))
        proper = seen.get('xfp') == 'https' and host in xfh and 'evil.example.test' not in xfh + str(seen.get('host')) and '203.0.113.9' not in xff + str(seen.get('forwarded'))
        self.cell(server, mode + '/C1-forwarded', 'ok' if proper and response.status == 200 else 'wrong',
                  'status %s host=%s xfp=%s xfh=%s xff=%s' % (response.status, seen.get('host'), seen.get('xfp'), xfh, xff))
        if not deep:
            return
        # F: an app that forces https unless told the visitor used it
        conn, response, _ = self.request(host, 'GET', '/force-ssl')
        response.read()
        conn.close()
        self.cell(server, mode + '/F-force-ssl', 'ok' if response.status == 200 else 'wrong', 'status %s' % response.status)
        # C8: the first SSE event arrives before the app is done (events are 0.7 s apart)
        conn, response, started = self.request(host, 'GET', '/sse', {'Accept': 'text/event-stream'})
        first = response.read(1)
        elapsed = time.time() - started
        response.read()
        conn.close()
        self.cell(server, mode + '/C8-sse', 'ok' if first and elapsed < 1.2 else 'buffered', 'first event after %d ms' % (elapsed * 1000))
        # C12: Range
        conn, response, _ = self.request(host, 'GET', '/range', {'Range': 'bytes=100-199'})
        data = response.read()
        conn.close()
        self.cell(server, mode + '/C12-range', 'ok' if response.status == 206 and len(data) == 100 else 'range_lost', 'status %s, %d bytes' % (response.status, len(data)))
        # C13: Expect: 100-continue with 5 MiB (curl, as a real client)
        done = subprocess.run(['curl', '-sS', '-m', '60', '-o', '/dev/null', '-w', '%{http_code}', '-X', 'POST', '-H', 'Expect: 100-continue',
                               '--data-binary', '@-', 'https://%s/upload' % host], input=b'x' * (5 << 20), capture_output=True)
        self.cell(server, mode + '/C13-expect', 'ok' if done.stdout == b'200' else 'app_refuses', 'status %s' % done.stdout.decode())

    # --- setup ----------------------------------------------------------------------------------------------
    def stage_windows(self):
        """The app of origin_proxy_matrix on the device itself (127.0.0.1:9000): the agent's dialer and upload path are the
        parts that differ per OS, so Windows runs the app cells, not the Docker proxies."""
        device = winhost.device()
        path = 'C:\\Users\\Public\\tun-app.py'
        with open(os.path.join(MATRIX, 'app.py'), 'rb') as app:
            device.write_bytes(path, app.read())
        device.ps('start app', "Get-CimInstance Win32_Process | Where-Object { $_.CommandLine -like '*tun-app.py*' } | ForEach-Object { Stop-Process -Id $_.ProcessId -Force }\n"
                  "$py=(Get-Command python).Source\n"
                  # created by WMI, outside the SSH session, which ends its own processes when it closes (see winhost.START)
                  "$r=Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{CommandLine=('\"'+$py+'\" '+$req.path+' 127.0.0.1')}\n"
                  "if($r.ReturnValue -ne 0){throw ('process create failed: '+$r.ReturnValue)}", path=path)
        time.sleep(3)

    def stage(self):
        if self.args.windows:
            return self.stage_windows()
        self.on_host('rm -rf ~/small-matrix && mkdir -p ~/small-matrix')
        subprocess.run(['scp', '-q', '-r', '-o', 'BatchMode=yes', MATRIX, '%s:small-matrix/' % self.args.host], check=True)
        self.docker('rm', '-f', 'tun-app', check=False)
        self.docker('run', '-d', '--name', 'tun-app', '--network', 'container:' + self.args.container,
                    '-v', self.home + '/small-matrix/origin_proxy_matrix/app.py:/app.py:ro', 'python:3-alpine', 'python', '/app.py', '127.0.0.1')
        self.containers.append('tun-app')

    def preview(self, url):
        """A preview of url (kind=preview); returns (host, preview id)."""
        data = self.pb('preview', url, '--background', '--ttl', '3h', timeout=240)
        self.previews.append(data['id'])
        return data['endpoint'].split('://', 1)[1].rstrip('/'), data['id']

    def tunnel(self, name, *origin):
        if self.args.kind == 'preview':
            target = origin[1] if origin[0] == '--from' else 'http://127.0.0.1:%s' % origin[1]
            return self.preview(target)
        data = self.pb('tunnel', 'create', name, *origin, '--duration', '3h', '--wait', '--timeout', '90s', timeout=240)
        tunnel = data['tunnel']
        self.tunnels.append(name)
        host = tunnel['stable_endpoint'].split('://', 1)[1].rstrip('/')
        routes = self.pb('tunnel', 'route', 'list', name)
        route = routes['items'][0]['id']
        return host, route

    def move(self, tunnel, route, origin, *flags):
        if self.args.kind == 'preview':
            if flags:
                raise RuntimeError('a preview has no origin TLS options: %s' % ' '.join(flags))
            self.pb('preview', 'stop', route, timeout=120)
            host, _ = self.preview(origin)
            self.moved_host = host
            time.sleep(4)
            return
        self.pb('tunnel', 'route', 'update', tunnel, route, '--to', origin, *flags, '--wait', '--timeout', '90s', timeout=240)
        time.sleep(4)

    def certs(self, host):
        directory = self.home + '/small-matrix/certs-' + host.split('.')[0]
        self.on_host('mkdir -p %s && openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj /CN=%s -addext subjectAltName=DNS:%s -keyout %s/leaf.key -out %s/leaf.crt 2>/dev/null && chmod 644 %s/leaf.key'
                     % (directory, host, host, directory, directory, directory))
        return directory

    # The paperboat daemon owns 127.100.0.1:80/443 inside the machine container, so each proxy binds 127.0.0.1 only.
    BIND = [r"s/^\(\s*\)admin off/\1admin off\n\1default_bind 127.0.0.1/", r"s/listen 80 /listen 127.0.0.1:80 /", r"s/listen 443 /listen 127.0.0.1:443 /",
            r"s/^Listen \(80\|443\)$/Listen 127.0.0.1:\1/", r"s/<VirtualHost \*:/<VirtualHost 127.0.0.1:/", r's/address: ":\(80\|443\)"/address: "127.0.0.1:\1"/']

    def render(self, template, host, name):
        path = self.home + '/small-matrix/%s' % name
        edits = ' '.join('-e ' + shlex.quote(item) for item in self.BIND)
        self.on_host("sed -e 's/@HOST@/%s/g' -e 's/app:9000/127.0.0.1:9000/g' %s %s/origin_proxy_matrix/%s > %s" % (host, edits, self.home + '/small-matrix', template, path))
        return path

    def run_proxy(self, name, image, mounts, env=(), command=()):
        self.docker('rm', '-f', 'tun-' + name, check=False)
        argv = ['run', '-d', '--name', 'tun-' + name, '--network', 'container:' + self.args.container]
        for item in env:
            argv += ['-e', item]
        for item in mounts:
            argv += ['-v', item]
        self.docker(*argv, image, *command)
        self.containers.append('tun-' + name)
        time.sleep(9)

    def stop_proxy(self, name):
        self.docker('rm', '-f', 'tun-' + name, check=False)

    def run(self):
        self.stage()
        base = self.home + '/small-matrix'
        # the app alone
        host, route = self.tunnel('tun-%s-app' % self.suffix, '--port', '9000')
        self.probe_all('app', 'direct-http', host)
        conn, response, started = self.request(host, 'GET', '/slow?seconds=%d' % self.args.slow, timeout=150) if self.args.slow else (None, None, None)
        if response is not None:
            response.read()
            conn.close()
            self.cell('app', 'direct-http/C11-slow', 'ok' if response.status == 200 else 'wrong', 'status %s after %d s' % (response.status, time.time() - started))
        for proxy in ([] if self.args.windows else self.args.only):
            self.proxy_cells(proxy, base)
        if not (self.args.windows or self.args.no_grpc):
            self.grpc_cells()

    def proxy_cells(self, proxy, base):
        name = 'tun-%s-%s' % (self.suffix, proxy)
        try:
            host, route = self.tunnel(name, '--port', '80' if self.args.kind == 'preview' else '9000')  # a preview is made for the proxy's own port
            tunnel = name
            certs = self.certs(host)
            if proxy in ('caddy-certs', 'caddy-internal'):
                template = {'caddy-certs': 'Caddyfile.certs', 'caddy-internal': 'Caddyfile.internal'}[proxy]
                path = self.render(template, host, name + '.conf')
                self.run_proxy(proxy, 'caddy:2', ['%s:/etc/caddy/Caddyfile:ro' % path, '%s:/certs:ro' % certs], env=['PUBLIC_HOST=' + host])
            elif proxy == 'traefik':
                dynamic = self.render('traefik-dynamic.yml.tmpl', host, name + '-dynamic.yml')
                self.run_proxy(proxy, 'traefik:v3', ['%s:/etc/traefik/traefik.yml:ro' % self.render('traefik-static.yml', host, name + '-static.yml'), '%s:/etc/traefik/dynamic.yml:ro' % dynamic, '%s:/certs:ro' % certs])
            elif proxy == 'nginx':
                path = self.render('nginx.conf.tmpl', host, name + '.conf')
                self.run_proxy(proxy, 'nginx:alpine', ['%s:/etc/nginx/nginx.conf:ro' % path, '%s:/certs:ro' % certs])
            elif proxy == 'httpd':
                path = self.render('httpd.conf.tmpl', host, name + '.conf')
                self.run_proxy(proxy, 'httpd:2.4-alpine', ['%s:/usr/local/apache2/conf/httpd.conf:ro' % path, '%s:/certs:ro' % certs])
            if self.args.kind != 'preview':
                self.move(tunnel, route, 'http://127.0.0.1:80')
            self.probe_all(proxy, 'a-http', host, expect_loop=True)
            if self.args.kind == 'preview':
                self.cell(proxy, 'b-https', 'blocked', 'a preview takes no origin TLS options, so a private-CA https origin cannot be previewed')
                return
            try:
                self.move(tunnel, route, 'https://127.0.0.1:443', '--tls-verification', 'insecure')
            except RuntimeError as refusal:
                # The deployed server only takes system/custom_ca verification until the insecure/pinned modes ship.
                self.cell(proxy, 'b-https', 'blocked', 'the server refused --tls-verification insecure: %s' % str(refusal)[-120:])
                return
            self.probe_all(proxy, 'b-https', host)
        except Exception as exc:
            self.cell(proxy, 'setup', 'error', exc)
        finally:
            self.stop_proxy(proxy)

    def grpc_cells(self):
        try:
            self.docker('rm', '-f', 'tun-grpc', check=False)
            self.docker('run', '-d', '--name', 'tun-grpc', '--network', 'container:' + self.args.container, 'python:3-alpine', 'sleep', 'infinity')
            self.containers.append('tun-grpc')
            self.docker('exec', 'tun-grpc', 'pip', 'install', '-q', '--no-cache-dir', 'grpcio', 'grpcio-health-checking', timeout=600)
            self.on_host('cat > ~/small-matrix/grpc_server.py <<"EOF"\n%s\nEOF\ncat > ~/small-matrix/grpc_client.py <<"EOF"\n%s\nEOF' % (GRPC_SERVER, GRPC_CLIENT))
            self.docker('cp', self.home + '/small-matrix/grpc_server.py', 'tun-grpc:/grpc_server.py')
            self.docker('cp', self.home + '/small-matrix/grpc_client.py', 'tun-grpc:/grpc_client.py')
            self.on_host('sudo docker exec -d tun-grpc python /grpc_server.py')
            time.sleep(3)
            host, route = self.tunnel('tun-%s-grpc' % self.suffix, '--from', 'h2c://127.0.0.1:50051')
            time.sleep(4)
            done = self.docker('exec', 'tun-grpc', 'python', '/grpc_client.py', host, check=False, timeout=90)
            try:
                result = json.loads(done.stdout.strip().splitlines()[-1])
            except (ValueError, IndexError):
                result = {'error': (done.stderr or done.stdout)[-200:]}
            unary = result.get('unary') == 'SERVING'
            stream = len(result.get('stream', [])) >= 2 and result['stream'][0] != result['stream'][-1]
            self.cell('grpc', 'h2c-direct/C9-unary', 'ok' if unary else 'failed', json.dumps(result)[:200])
            self.cell('grpc', 'h2c-direct/C9-stream', 'ok' if stream else 'failed', json.dumps(result.get('stream', result.get('error', '')))[:200])
        except Exception as exc:
            self.cell('grpc', 'setup', 'error', exc)

    def cleanup(self):
        for name in self.tunnels:
            try:
                if self.args.windows:
                    preview = winhost.pb_path().run_remote(['tunnel', 'delete', name, '--json'], timeout=120)
                    token = json.loads(preview.stdout)['data']['confirmation_token']
                    winhost.pb_path().run_remote(['tunnel', 'delete', name, '--json', '--confirm', token], timeout=120)
                    continue
                preview = self.inside(self.args.pb, 'tunnel', 'delete', name, '--json', check=False)
                token = json.loads(preview.stdout)['data']['confirmation_token']
                self.inside(self.args.pb, 'tunnel', 'delete', name, '--json', '--confirm', token, check=False)
            except Exception as exc:
                print('cleanup: tunnel %s not deleted: %s' % (name, exc), file=sys.stderr)
        for identity in self.previews:
            try:
                self.inside(self.args.pb, 'preview', 'stop', identity, '--json', check=False, timeout=120)
            except Exception as exc:
                print('cleanup: preview %s not stopped: %s' % (identity, exc), file=sys.stderr)
        if self.args.windows:
            winhost.device().ps('stop app', "Get-CimInstance Win32_Process | Where-Object { $_.CommandLine -like '*tun-app.py*' } | ForEach-Object { Stop-Process -Id $_.ProcessId -Force }")
            return
        for name in self.containers:
            self.docker('rm', '-f', name, check=False)


def table(cells):
    rows = {}
    for cell in cells:
        server = cell['server']
        rows.setdefault(server, []).append('%s=%s' % (cell['mode'], cell['result']))
    for server, items in rows.items():
        print('%-16s %s' % (server, '  '.join(items)))


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--windows', action='store_true', help='drive the Windows device selected by SMOKE_WINDOWS_SSH and PB_SMOKE_WINDOWS_PB: the app cells only')
    parser.add_argument('--no-grpc', action='store_true')
    parser.add_argument('--host', default='', help='ssh alias of the Docker host that runs the machine container (not with --windows)')
    parser.add_argument('--container', default='', help='the paired machine container (prefix small-; not with --windows)')
    parser.add_argument('--pb', default='/home/pbt/.local/bin/pb', help='pb inside the machine container')
    parser.add_argument('--only', nargs='*', default=['caddy-certs', 'caddy-internal', 'traefik', 'nginx', 'httpd'])
    parser.add_argument('--slow', type=int, default=45, help='seconds of first-byte delay for the C11 cell (0 skips)')
    parser.add_argument('--kind', choices=['tunnel', 'preview'], default='tunnel', help='publish through durable tunnels or temporary previews')
    parser.add_argument('--out', required=True)
    parser.add_argument('--keep', action='store_true')
    args = parser.parse_args()
    if not args.windows and not (args.host and args.container):
        parser.error('--host and --container are required unless --windows')
    lab = Lab(args)
    try:
        lab.run()
    finally:
        with open(args.out, 'w') as out:
            json.dump({'check': 'origin-tunnel-matrix', 'cells': lab.cells}, out, indent=1)
        if not args.keep:
            lab.cleanup()
    table(lab.cells)
    bad = [c for c in lab.cells if c['result'] in ('error', 'visitor_error', 'wrong', 'unexpected', 'failed')]  # 'blocked' cells wait for a server deploy
    sys.exit(1 if bad else 0)


if __name__ == '__main__':
    main()
