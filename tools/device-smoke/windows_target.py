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
PIECE = 800  # base64 characters of a staged script per pb exec: keeps each command under pb's 4096-character limit


def is_windows(binary, target):
    """Whether the machine `target` (alias or id) reports platform windows in the account's machine list."""
    key = (str(binary), target)
    if key not in _platforms:
        from pairing_lifecycle import invoke
        machines = invoke(binary, '--workspace', 'personal', 'machine', 'list', '--all', '--json')['machines']
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
    # pb exec refuses a command of more than 4096 characters, and -EncodedCommand triples a script's length, so the
    # script goes over as base64 pieces appended to a file and is decoded there.
    name = 'pb-smoke-' + uuid.uuid4().hex
    text = b64(with_preamble(script))
    for start in range(0, len(text), PIECE):
        run(binary, 'exec', target, '--timeout', '60s', '--', *ps_argv(
            "Add-Content -LiteralPath (Join-Path $env:TEMP '%s.b64') -Value '%s' -Encoding ascii" % (name, text[start:start + PIECE])))
    decode = ("$b=Join-Path $env:TEMP '%s.b64';$p=Join-Path $env:TEMP '%s.ps1';"
              "[IO.File]::WriteAllText($p,[Text.Encoding]::UTF8.GetString([Convert]::FromBase64String((Get-Content -LiteralPath $b -Raw))),"
              "(New-Object Text.UTF8Encoding($true)));Remove-Item -LiteralPath $b -Force;[Console]::Out.Write($p)" % (name, name))
    path = run(binary, 'exec', target, '--timeout', '60s', '--', *ps_argv(decode))[0].decode().strip()
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


# ---- the small sh commands the ENV, team and policy checks run on their target --------------------------------

def target_argv(binary, target, unix_argv, script):
    """unix_argv for a Unix target, the PowerShell script in its place for a Windows one."""
    return ps_argv(script) if is_windows(binary, target) else unix_argv


def true_argv(binary, target):
    return ['cmd', '/c', 'exit', '0'] if is_windows(binary, target) else ['true']


def _env_name(name):
    assert re.fullmatch(r'[A-Za-z_][A-Za-z0-9_]*', name), name
    return name


def printenv_script(name):
    """printenv NAME: the value and a newline, or exit 1 when it is unset."""
    return ("$v=[Environment]::GetEnvironmentVariable('%s')\nif($null -eq $v){exit 1}\n"
            '[Console]::Out.Write($v+"`n")' % _env_name(name))


def bracket_env_script(name):
    """sh -c 'echo "[$NAME]"'."""
    return "[Console]::Out.Write('['+[Environment]::GetEnvironmentVariable('%s')+\"]`n\")" % _env_name(name)


def diagnostics_scan_script(values):
    """grep -rlF over the daemon's diagnostics directory: prints the files that contain one of the values."""
    return ("$d=Join-Path (Join-Path (Join-Path $env:LOCALAPPDATA 'Paperboat') 'state') 'diagnostics'\n"
            "Get-ChildItem -LiteralPath $d -Recurse -File -ErrorAction SilentlyContinue|ForEach-Object{\n"
            "$f=[IO.File]::Open($_.FullName,'Open','Read','ReadWrite');$t=(New-Object IO.StreamReader($f)).ReadToEnd();$f.Close()\n"
            "foreach($v in @(%s)){if($t.Contains($v)){[Console]::Out.Write($_.FullName+\"`n\");break}}}" % _names(values))


def env_marker_line(marker, name):
    """Typed at the cmd.exe prompt of a session: prints <marker><<value of NAME>> and ends the shell."""
    return typed_marker(marker, "+'<'+$env:%s+'>'" % _env_name(name)) + ' & exit'


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


# ---- managed SSH to a Windows target ----------------------------------------------------------------------------
# Windows OpenSSH hands a remote command to cmd.exe (no DefaultShell is configured by Paperboat) and starts cmd.exe
# as the interactive shell; there is no sh, printf, id, python3 or rsync.

def ssh_command(script):
    """The one command string of `pb ssh <target> -- <string>` (or plain ssh) that runs script in PowerShell."""
    return ' '.join(ps_argv(script))


def exact_output_script(out_text, err_text, code):
    """Writes the exact UTF-8 bytes of out_text to stdout and of err_text to stderr, then exits with code."""
    return ("$o=[Console]::OpenStandardOutput();$b=[Convert]::FromBase64String('%s');$o.Write($b,0,$b.Length);$o.Flush()\n"
            "$e=[Console]::OpenStandardError();$b=[Convert]::FromBase64String('%s');$e.Write($b,0,$b.Length);$e.Flush()\n"
            "exit %d" % (b64(out_text), b64(err_text), code))


def typed_marker(token, expression=''):
    """A line to type at a Windows prompt (cmd.exe or PowerShell) that prints token on a line of its own.

    The console echoes what is typed, so the typed text never contains token itself. expression is PowerShell
    appended to the token (for example +$env:NAME)."""
    assert re.fullmatch(r'[A-Za-z0-9_]{4,}', token), token
    return typed_line("Write-Output ('%s'+'%s'%s)" % (token[:3], token[3:], expression))


def username_script():
    return "[Console]::Out.Write($env:USERNAME)"


def _tools_root(name):
    if not re.fullmatch(r'smoke-ssh-tools-[0-9a-f]{32}', name):
        raise ValueError('not a directory name of the SSH tools check')
    return "$r=Join-Path (Join-Path $env:USERPROFILE 'Paperboat') '%s'\n" % name


def tools_setup_script(name):
    """Creates ~/Paperboat/<name>/scp (it must not exist); scp and sftp then use the home-relative path."""
    return (_tools_root(name) + "if(Test-Path -LiteralPath $r){throw 'the fixture directory already exists'}\n"
            "[void](New-Item -ItemType Directory -Path (Join-Path $r 'scp'))\n[Console]::Out.Write('created')")


def tools_cleanup_script(name):
    return _tools_root(name) + "Remove-Item -LiteralPath $r -Recurse -Force"


def long_script(seconds, marker, step=60):
    """A line every `step` seconds until `seconds` have passed, then the marker (ssh_long's sh loop)."""
    return ("$i=0;while($i -lt %d){Start-Sleep -Seconds %d;$i+=%d;[Console]::Out.Write(\"t$i`n\");[Console]::Out.Flush()}\n"
            "[Console]::Out.Write(\"%s`n\")" % (seconds, step, step, marker))


# ---- lines typed into a terminal session on a Windows target ---------------------------------------------------
# The session shell is cmd.exe under ConPTY (the host runtime's PAPERBOAT_SHELL). It has no sh tools, so each step
# is one PowerShell line typed there. Inside the double quotes cmd.exe leaves everything but % alone.

def typed_line(script):
    assert not re.search(r'["%\r\n]', script), script
    return 'powershell -NoProfile -Command "%s"' % script


def count_line(prefix, last, slow=False):
    """Prints <prefix>1 .. <prefix>last, one per line; slow: one a second."""
    return typed_line("1..%d|ForEach-Object{'%s'+$_%s}" % (last, prefix, ';Start-Sleep 1' if slow else ''))


def flood_line(lines=None):
    """`lines` lines of 49 characters (about 51 bytes each), or such lines without end. Lines of `y` are no flood on a
    ConPTY: a loop of those runs at 2-5 KB/s even without pb (measured on the Windows PC over plain ssh, X-24)."""
    loop = "while(1)" if lines is None else "for($i=0;$i -lt %d;$i++)" % lines
    return typed_line("$l='0123456789012345678901234567890123456789012345678';%s{[Console]::Out.WriteLine($l)}" % loop)


def sink_line():
    """Reads console lines until end of input (Ctrl-Z, Enter) and prints nothing: cat > /dev/null."""
    return typed_line("while($null -ne [Console]::In.ReadLine()){}")


def size_line():
    """Prints `PB_SIZE <rows> <columns>` of the console window (stty size)."""
    return typed_line("$s=$Host.UI.RawUI.WindowSize;Write-Output ('PB_'+'SIZE '+$s.Height+' '+$s.Width)")


def colour_line():
    """Prints PB_COLOUR in red (SGR 31) and resets."""
    return typed_line("Write-Output ([char]27+'[31mPB_'+'COLOUR'+[char]27+'[0m')")


def foreground_line(token, seconds):
    """A foreground program that prints token, then sleeps."""
    assert re.fullmatch(r'[A-Za-z0-9_]{4,}', token), token
    return typed_line("Write-Output ('%s'+'%s');Start-Sleep %d" % (token[:3], token[3:], seconds))


def leaf(path):
    """The last component of a Windows path (the unique directory name a run created)."""
    return re.split(r'[\\/]', str(path).rstrip('\\/'))[-1]
