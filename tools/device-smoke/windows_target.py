"""Commands a check runs on its TARGET machine, for a Windows target: PowerShell in place of python3/sh/ps/printf.

The checks reach the target through `pb exec <target> -- <argv>`. A Unix target gets python3 and sh; a Windows target
has neither, so these build the equivalent argv. Short scripts go through -EncodedCommand; a script whose arguments
must arrive exactly as pb passed them is staged as a .ps1 file and run with -File, so Windows' own argument parsing
is what the check exercises (-EncodedCommand cannot take arguments).
"""
import base64
import contextlib
import functools
import os
import re
import uuid

import winhost

# Comparisons of exact values use PowerShell's case-sensitive operators (-ceq/-cne/-cmatch): Unix compares exactly.
PREAMBLE = "$ProgressPreference='SilentlyContinue';$ErrorActionPreference='Stop';"
INBOX = "(Join-Path (Join-Path $env:USERPROFILE 'Paperboat') 'Inbox')"
_platforms = {}


def is_windows(binary, target):
    """Whether the machine `target` (alias or id) reports platform windows in the account's machine list."""
    key = (str(binary), target)
    if key not in _platforms:
        from pairing_lifecycle import invoke
        machines = invoke(binary, '--workspace', 'personal', 'machine', 'list', '--json')['machines']
        match = next((m for m in machines if target in (m['id'], m['alias'])), None)
        _platforms[key] = bool(match) and str(match.get('platform', '')).lower().startswith('windows')
    return _platforms[key]


def b64(text):
    return base64.b64encode(text.encode()).decode()


def with_preamble(script):
    """PREAMBLE first, except after a leading param() block, which PowerShell requires to be the first statement."""
    if script.startswith('param('):
        head, _, rest = script.partition('\n')
        return head + '\n' + PREAMBLE + rest
    return PREAMBLE + script


def ps_argv(script):
    encoded = base64.b64encode(with_preamble(script).encode('utf-16-le')).decode()
    return ['powershell', '-NoProfile', '-NonInteractive', '-InputFormat', 'Text', '-OutputFormat', 'Text',
            '-EncodedCommand', encoded]


@contextlib.contextmanager
def staged(binary, target, script):
    """Write script to a .ps1 on the target (UTF-8 with BOM, so Windows PowerShell reads Unicode) and yield its path."""
    from exec_lifecycle import run
    writer = ("$p=Join-Path $env:TEMP ('pb-smoke-'+[guid]::NewGuid().ToString('N')+'.ps1');"
              "[IO.File]::WriteAllText($p,[Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('%s')),"
              "(New-Object Text.UTF8Encoding($true)));[Console]::Out.Write($p)" % b64(with_preamble(script)))
    path = run(binary, 'exec', target, '--timeout', '60s', '--', *ps_argv(writer))[0].decode().strip()
    try:
        yield path
    finally:
        run(binary, 'exec', target, '--timeout', '60s', '--', *ps_argv(
            "Remove-Item -LiteralPath '%s' -Force -ErrorAction SilentlyContinue" % path.replace("'", "''")))


def file_argv(path, *arguments):
    return ['powershell', '-NoProfile', '-NonInteractive', '-File', path, *arguments]


def target_pb(binary, target):
    """The pb.exe path on a Windows target: PB_SMOKE_TARGET_WINDOWS_PB, else the one on its PATH."""
    given = os.environ.get('PB_SMOKE_TARGET_WINDOWS_PB')
    if given:
        return given
    from exec_lifecycle import run
    out = run(binary, 'exec', target, '--timeout', '60s', '--', *ps_argv(
        "[Console]::Out.Write((Get-Command pb -ErrorAction Stop).Source)"))[0].decode().strip()
    if not out:
        raise RuntimeError('pb.exe is not on the Windows target PATH; set PB_SMOKE_TARGET_WINDOWS_PB')
    return out


# ---- scripts for exec_lifecycle -------------------------------------------------------------------------------

def exec_script(argument, digest, unicode_line, stderr_line, exit_code):
    """Checks its argument and the sha256 of stdin, then writes 16 MiB of 0..255 repeated, a Unicode line and a
    stderr line, and exits with exit_code (the Windows form of exec_lifecycle's python3 -c fixture)."""
    return ("param([string]$a)\n"
            "$want=[Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('%s'))\n"
            "if($a -cne $want){exit 91}\n"
            "$m=New-Object IO.MemoryStream;[Console]::OpenStandardInput().CopyTo($m)\n"
            "$h=-join([Security.Cryptography.SHA256]::Create().ComputeHash($m.ToArray())|ForEach-Object{$_.ToString('x2')})\n"
            "if($h -cne '%s'){exit 92}\n"
            "$o=[Console]::OpenStandardOutput();$blk=New-Object byte[] 65536\n"
            "for($j=0;$j -lt 65536;$j++){$blk[$j]=$j%%256}\n"
            "for($k=0;$k -lt 256;$k++){$o.Write($blk,0,$blk.Length)}\n"
            "$u=[Convert]::FromBase64String('%s');$o.Write($u,0,$u.Length);$o.Flush()\n"
            "$e=[Console]::OpenStandardError();$s=[Convert]::FromBase64String('%s');$e.Write($s,0,$s.Length);$e.Flush()\n"
            "exit %d" % (b64(argument), digest, b64(unicode_line), b64(stderr_line), exit_code))


def zeros_script(size):
    return ("$o=[Console]::OpenStandardOutput();$b=New-Object byte[] 1048576\n"
            "for($i=0;$i -lt %d;$i++){$o.Write($b,0,$b.Length)};$o.Flush()" % (size >> 20))


def out_err_script(out_text, err_text, code):
    # double quotes: the caller's `n is a newline (no $ in these texts)
    return ('[Console]::Out.Write("%s");[Console]::Out.Flush();[Console]::Error.Write("%s");[Console]::Error.Flush();exit %d'
            % (out_text, err_text, code))


def print_script(text):
    return "[Console]::Out.Write('%s')" % text.replace("'", "''")


# ---- scripts for exec_cancel ----------------------------------------------------------------------------------

def sleeper_script():
    """Prints `<nonce> <pid>` and sleeps two minutes (the Windows form of exec_cancel's python3 -c fixture)."""
    return "param([string]$n)\n[Console]::Out.Write(\"$n $PID`n\");[Console]::Out.Flush();Start-Sleep -Seconds 120"


def stopped_script(pid, nonce):
    return ("$d=(Get-Date).AddSeconds(5)\n"
            "while((Get-Date) -lt $d){$c=(Get-CimInstance Win32_Process -Filter 'ProcessId=%d').CommandLine\n"
            "if(-not $c -or $c -cnotmatch '%s'){exit 0};Start-Sleep -Milliseconds 100}\n"
            "throw 'canceled remote process remains alive'" % (int(pid), re.escape(nonce)))


def kill_script(pid, nonce):
    return ("$c=(Get-CimInstance Win32_Process -Filter 'ProcessId=%d').CommandLine\n"
            "if($c -cmatch '%s'){Stop-Process -Id %d -Force}" % (int(pid), re.escape(nonce), int(pid)))


# ---- scripts for the transfer checks: the receiving Inbox ------------------------------------------------------

def _names(names):
    return ','.join(winhost.ps_quote(name) for name in names)


def inbox_cleanup_script(prefix, names):
    return ("$r=%s\nforeach($n in @(%s)){if(-not $n.StartsWith(%s)){throw 'refusing a name this check did not create'}\n"
            "Remove-Item -LiteralPath (Join-Path $r $n) -Force -ErrorAction SilentlyContinue}"
            % (INBOX, _names(names), winhost.ps_quote(prefix)))


def inbox_cleanup_glob_script(prefix):
    return ("$r=%s\nGet-ChildItem -LiteralPath $r -Filter %s -ErrorAction SilentlyContinue|Remove-Item -Force"
            % (INBOX, winhost.ps_quote(prefix + '*')))


def inbox_verify_script(expected, marker, size=None, glob=None):
    """Every name in expected arrived with that sha256 (and `size` bytes); with glob, nothing else matches it."""
    pairs = ';'.join('%s=%s' % (winhost.ps_quote(name), winhost.ps_quote(digest)) for name, digest in expected.items())
    check = ("$r=%s\n$e=@{%s}\n" % (INBOX, pairs))
    if glob:
        check += ("$f=@(Get-ChildItem -LiteralPath $r -Filter %s|Sort-Object Name|ForEach-Object{$_.Name})\n"
                  "if((($f -join '|') -cne (($e.Keys|Sort-Object) -join '|'))){throw ('unexpected files: '+($f -join ','))}\n"
                  % winhost.ps_quote(glob + '*'))
    check += ("foreach($n in $e.Keys){$p=Join-Path $r $n\n"
              "if(-not(Test-Path -LiteralPath $p)){throw ('missing '+$n)}\n")
    if size is not None:
        check += "if((Get-Item -LiteralPath $p).Length -ne %d){throw ('wrong size '+$n)}\n" % size
    check += ("if((Get-FileHash -LiteralPath $p -Algorithm SHA256).Hash.ToLower() -cne $e[$n]){throw ('hash '+$n)}}\n"
              '[Console]::Out.Write("%s`n")' % marker)  # the Unix fixtures print the marker with print(): a newline follows
    return check


def restart_service_detached_script(pb_exe):
    """Restart this machine's daemon a second from now without waiting, as the Unix `(sleep 1; pb service restart)&`."""
    command = "Start-Sleep 1; & %s service restart" % winhost.ps_quote(pb_exe)
    return ("Start-Process -WindowStyle Hidden -FilePath powershell.exe -ArgumentList '-NoProfile','-NonInteractive',"
            "'-EncodedCommand','%s'" % base64.b64encode(command.encode('utf-16-le')).decode())
