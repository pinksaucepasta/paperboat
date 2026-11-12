#!/usr/bin/env python3
"""F21-160 on a Windows device: a dead direct path is noticed in seconds, not by the 2 minute QUIC idle timeout.

The Windows device (SMOKE_WINDOWS_SSH, PB_SMOKE_WINDOWS_PB, VICTUS_PASS for a password login) runs one long
`pb exec PEER -- sh -c 'while true; do echo; sleep 0.5; done'` and timestamps every line as it arrives on the device. The
direct path to PEER (a Unix machine of the account on the same LAN) is then dropped with a firewall block of pb.exe's
traffic to --peer-address (winhost.cut_network: pb.exe only, so the SSH that drives the device keeps working, with a
timer on the device that lifts the rule whatever happens here). The stream must go on within --budget seconds (the
relay path takes over); the longest gap between lines is the result. Prints one JSON receipt.
"""
import argparse
import json
import sys
import time

import winhost

STREAM = ("$i=New-Object Diagnostics.ProcessStartInfo;$i.FileName=$req.pb;$i.Arguments=$req.args;$i.UseShellExecute=$false\n"
          "$i.RedirectStandardOutput=$true;$i.RedirectStandardInput=$true;$i.CreateNoWindow=$true\n"
          "$p=[Diagnostics.Process]::Start($i);$p.StandardInput.Close()\n"
          "$w=[IO.File]::AppendText($req.out);$w.AutoFlush=$true\n"
          "while(($l=$p.StandardOutput.ReadLine()) -ne $null){$w.WriteLine([DateTime]::UtcNow.ToString('o')+' '+$l)}\n")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('peer', help='machine name of the Unix peer')
    parser.add_argument('--peer-address', required=True, help='the peer host address the direct path runs to (blocked)')
    parser.add_argument('--budget', type=float, default=15.0, help='seconds the stream may stall after the cut')
    parser.add_argument('--cut-seconds', type=int, default=30)
    parser.add_argument('--output', default='')
    args = parser.parse_args()
    device = winhost.device()
    if not device:
        sys.exit('PB_SMOKE_WINDOWS_PB (and SMOKE_WINDOWS_SSH) must name the Windows device')
    pb = winhost.pb_path()
    steps = []

    def step(name, ok, detail=''):
        steps.append({'step': name, 'ok': bool(ok), 'detail': str(detail)[:300]})
        print(('PASS ' if ok else 'FAIL ') + name, '-', str(detail)[:200], flush=True)

    deadline = time.time() + 120
    direct = False
    while time.time() < deadline and not direct:
        pb.run_remote(['exec', args.peer, '--', 'true'], timeout=60)
        text = pb.run_remote(['status'], timeout=60).stdout
        block = text.split(args.peer, 1)[-1][:400] if args.peer in text else ''
        direct = 'Path: direct' in block.split('\n\n')[0]
        time.sleep(3)
    step('a direct path to the peer exists before the cut', direct, 'pb status Path: direct')
    if not direct:
        return finish(steps, args, {})
    cut = None
    with device.temp_dir('pb-direct-cut') as directory:
        raw, out = str(directory / 'raw.txt'), str(directory / 'lines.txt')
        script = '$req=@{pb=%s;args=%s;raw=%s;out=%s};' % (winhost.ps_quote(str(pb)), winhost.ps_quote(
            'exec %s --timeout 10m -- sh -c "while true; do echo tick; sleep 0.5; done"' % args.peer), winhost.ps_quote(raw), winhost.ps_quote(out)) + STREAM
        pid = device.start(['powershell.exe', '-NoProfile', '-NonInteractive', '-EncodedCommand', winhost._b64(script.encode('utf-16-le'))], directory)
        try:
            time.sleep(15)
            before = lines(device, out)
            step('the stream runs before the cut', len(before) >= 15, '%d lines in 15 s' % len(before))
            cut_at = time.time()
            cut = device.cut_network(args.cut_seconds + 60, addresses=[args.peer_address])
            time.sleep(args.cut_seconds)
            cut.release()
            cut = None
            time.sleep(10)
            rows = lines(device, out)
            gaps = [(b - a) for a, b in zip(rows, rows[1:])]
            worst = max(gaps, default=0.0)
            step('the stream continues after the direct path is cut', len(rows) > len(before) + 20, '%d lines in all' % len(rows))
            step('the longest gap in the stream stays under the budget (a dead path is noticed in seconds)', worst <= args.budget,
                 'longest gap %.1f s, budget %.1f s, 2 min QUIC idle timeout is the number to beat' % (worst, args.budget))
            return finish(steps, args, {'lines': len(rows), 'longest_gap_s': round(worst, 1), 'cut_seconds': args.cut_seconds})
        finally:
            if cut:
                cut.release()
            device.stop(pid)


def lines(device, path):
    """Arrival times (epoch seconds, device clock) of the lines logged so far."""
    import datetime
    text = device.ps('read the stream log', "if(Test-Path -LiteralPath $req.p){Get-Content -LiteralPath $req.p}", p=path)[1]
    stamps = []
    for line in text.splitlines():
        try:
            stamps.append(datetime.datetime.fromisoformat(line.split(' ', 1)[0].replace('Z', '+00:00')[:26] + '+00:00').timestamp())
        except ValueError:
            continue
    return stamps


def finish(steps, args, extra):
    ok = all(s['ok'] for s in steps)
    receipt = {'check': 'direct-path-cut', 'peer': args.peer, 'status': 'PASS' if ok else 'FAIL', 'steps': steps, **extra}
    print(json.dumps(receipt))
    if args.output:
        open(args.output, 'w').write(json.dumps(receipt, indent=1) + '\n')
    return 0 if ok else 1


if __name__ == '__main__':
    sys.exit(main())
