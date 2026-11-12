"""One machine under test for edge probe checks (ip_rules_rows.py, flood_rows.py): its pb and a loopback origin, on a Unix
device reached through a command prefix (`ssh host`, `ssh host sudo docker exec ...`) or on the Windows device selected by
SMOKE_WINDOWS_SSH and PB_SMOKE_WINDOWS_PB."""
import json
import shlex
import subprocess
from pathlib import Path

HERE = Path(__file__).resolve().parent
APP = HERE / 'origin_proxy_matrix' / 'app.py'
WINDOWS_STOP = "Get-CimInstance Win32_Process | Where-Object {{ $_.CommandLine -like '*{tag}.py*' }} | ForEach-Object {{ Stop-Process -Id $_.ProcessId -Force }}"


class Failure(Exception):
    pass


def remote(prefix, argv):
    """prefix + argv; through ssh the whole command is one string the remote shell parses, so it is quoted."""
    return prefix + ([shlex.join(argv)] if prefix and prefix[0] == 'ssh' else list(argv))


def run(argv, timeout=120, input=None):
    done = subprocess.run(argv, capture_output=True, text=True, timeout=timeout, input=input)
    return done.returncode, done.stdout, done.stderr


class Machine:
    def __init__(self, prefix, pb, windows=False):
        self.prefix, self.pb_path, self.windows = shlex.split(prefix) if prefix else [], pb, windows
        if windows:
            import winhost
            self.device, self.windows_pb = winhost.device(), winhost.pb_path()
            if not self.device:
                raise Failure('--windows needs SMOKE_WINDOWS_SSH')
        elif not self.prefix:
            raise Failure('--machine-cmd or --windows is required')

    def pb(self, *arguments, timeout=180):
        """(exit code, the command's data, or its text when it printed no JSON)."""
        if self.windows:
            done = self.windows_pb.run_remote(list(arguments), timeout=timeout)
            code, out, err = done.returncode, done.stdout, done.stderr
        else:
            code, out, err = run(remote(self.prefix, [self.pb_path, *arguments]), timeout)
        try:
            value = json.loads(out)
            value = value.get('data', value) if isinstance(value, dict) else value
        except ValueError:
            value = out.strip() or err.strip()
        return code, value

    def run_command(self, argv, timeout=30):
        """(exit code, stdout, stderr) of a command on the machine."""
        if self.windows:
            done = self.device.run(list(argv), timeout=timeout)
            return done.returncode, done.stdout, done.stderr
        return run(remote(self.prefix, list(argv)), timeout)

    def start_origin(self, port, tag, source=None):
        """origin_proxy_matrix/app.py on 127.0.0.1:port of the machine (the daemon owns 127.100.0.x ports for local access, so
        loopback only); returns the function that stops it. source is the origin's own program, with __PORT__ for the port."""
        app = source.replace('__PORT__', str(port)) if source else APP.read_text().replace('(sys.argv[1] if len(sys.argv) > 1 else "0.0.0.0", 9000)', '("127.0.0.1", %s)' % port)
        if self.windows:
            path = 'C:\\Users\\Public\\%s.py' % tag
            stop = WINDOWS_STOP.format(tag=tag)
            self.device.write_bytes(path, app.encode())
            # created by WMI, outside the SSH session, which ends its own processes when it closes (see winhost.START)
            self.device.ps('start origin', stop + "\n$py=(Get-Command python).Source\n"
                           "$r=Invoke-CimMethod -ClassName Win32_Process -MethodName Create -Arguments @{CommandLine=('\"'+$py+'\" '+$req.path)}\n"
                           "if($r.ReturnValue -ne 0){throw ('process create failed: '+$r.ReturnValue)}", path=path)
            return lambda: self.device.ps('stop origin', stop)
        code, _, err = run(remote(self.prefix, ['sh', '-c', 'cat > /tmp/%s.py && (nohup python3 /tmp/%s.py >/dev/null 2>&1 &)' % (tag, tag)]), 60, input=app)
        if code != 0:
            raise Failure('origin did not start: ' + err[:200])
        return lambda: run(remote(self.prefix, ['sh', '-c', 'pkill -f /tmp/%s.py; rm -f /tmp/%s.py' % (tag, tag)]), 30)
