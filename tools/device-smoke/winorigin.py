"""Origin web servers on the Windows host of a port-names check (port_names.py, https_front_desk.py).

The check logic stays on the controller; only a small python script (python is on every runner and on Windows test machine) runs on the
device, detached, with its output in a file the controller polls. {dir} in an argument is the script's temp directory; arguments have no spaces.
"""
import contextlib
import time

# Any path answers the marker given as argv[1], on 127.0.0.1 and a free port printed on the first line.
PAGE = r'''
import http.server, sys
class Page(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        body = sys.argv[1].encode()
        self.send_response(200); self.send_header('Content-Length', str(len(body))); self.end_headers(); self.wfile.write(body)
    log_message = lambda *a: None
server = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Page)
print(server.server_address[1], flush=True)
server.serve_forever()
'''

# Created by WMI, outside the SSH session: a Start-Process child dies when the session closes (see winhost.START).
# cmd.exe redirects the script's output; the process id is cmd's, and taskkill /T takes python with it.
START = r"""
$d=[string]$req.dir
[IO.File]::WriteAllText("$d\o.py",[string]$req.code)
$py=(Get-Command python).Source
$line='cmd.exe /c ""'+$py+'" "'+$d+'\o.py" '+(@($req.args) -join ' ')+' > "'+$d+'\out" 2> "'+$d+'\err""'
$r=Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{CommandLine=$line}
if($r.ReturnValue -ne 0){throw ('process create failed: '+$r.ReturnValue)}
$r.ProcessId
"""
STOP = "& taskkill.exe /T /F /PID $req.pid | Out-Null; exit 0"


@contextlib.contextmanager
def origin(device, script, *args, files=None, wait=30):
    """Run `script` with python on the device until the block ends; yield the first line it prints."""
    with device.temp_dir('pb-origin') as directory:
        for name, data in (files or {}).items():
            (directory / name).write_bytes(data)
        pid = device.ps('start origin', START, dir=str(directory), code=script,
                        args=[str(a).format(dir=directory) for a in args])[1].splitlines()[-1]
        try:
            deadline = time.monotonic() + wait
            while True:
                try:
                    line = (directory / 'out').read_text().strip()
                except FileNotFoundError:
                    line = ''
                if line:
                    break
                if time.monotonic() >= deadline:
                    raise RuntimeError('the origin server did not start: ' + _error(directory))
                time.sleep(1)
            yield line.splitlines()[0]
        finally:
            device.ps('stop origin', STOP, pid=int(pid))


def _error(directory):
    try:
        return (directory / 'err').read_text().strip()[-300:]
    except FileNotFoundError:
        return 'no output'
