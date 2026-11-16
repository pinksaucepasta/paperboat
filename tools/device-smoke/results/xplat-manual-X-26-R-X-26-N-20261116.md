# X-26 sleep / wake by freeze + network cut (lane R-X-26-N, 2026-11-16)

Stand-in for sleep (user decision 2026-11-16): freeze every pb process of the sleeping machine (SIGSTOP on Linux/macOS,
NtSuspendProcess on Windows) AND cut its pb network for 240 s (Linux: iptables owner drop of the pb user; macOS: pf anchor
`block drop out quick on en0 all user admin`; Windows: firewall block of every pb.exe, both directions), then resume both.
The machine is set to allow-sleep (Linux, Windows; the fleet Mac's own mode is left alone). Linux sleeper: systemd's
WatchdogSec is set to 0 on the paperboat units, because SIGSTOP trips the 30 s watchdog and systemd aborts the frozen daemon
(a real sleep stops that clock). Scripts: scratchpad driver, not committed (freeze/thaw + one run per sleeper).

Builds and machines: L = Linux container rx26n-l on pbvm2 (own account), W = winpool-3 hosted Windows runner rx26n-w (same account), both
installed over by the build named; M = the fleet Mac lanex2-m with the fleet Linux lanex2-l2 as its peer (FLEET4/me installed the build).
Each run: terminals open to and from the sleeper (typed before, typed WHILE frozen, typed after), a plain `pb preview --background`
(no --ttl) and a durable tunnel on the sleeper, one send in flight to it and one from it, a held-session reattach, a fresh send after.

## Final passing runs (every part passed)
- L sleeper, peer W, dev-6e1e23255, 13:19-13:28Z (and dev-caf37d9c6 12:46Z): offline with reason "allowed to sleep" seen from W after 99 s; exec
  refused with a clear error while away; exec back 3.7-7.2 s; list online 6.3 s; terminals typed-while-frozen ran once (count 1) and live
  8.8-13.0 s after thaw; held-session reattach replay + live ok; tunnel 6.9 s; plain preview 53 s; both in-flight sends published
  (300 MB L>W, 40 MB W>L); fresh send ok.
- W sleeper, peer L, dev-6e1e23255, 14:39-14:51Z: offline 75 s with reason; exec back 12-13 s; terminals once, live 15-17 s; reattach ok;
  tunnel 6.0 s; preview 14.1 s; sends published (300 MB L>W, 40 MB W>L); fresh send ok. Windows freeze = NtSuspendProcess of every pb.exe.
- M sleeper (fleet lanex2-m, peer lanex2-l2), parts on the builds named, none of the passing parts re-broken by later commits (git log
  caf37d9c6..6e1e23255 touches no terminal client, exec or preview path; the 499 fix is the only send change):
  dev-caf37d9c6 12:50Z: offline 104 s with reason, exec back 2-3 s, terminals both directions typed-while-frozen once, live 3.7-4.8 s, reattach
  ok, tunnel 7 s, preview 18 s, M>L2 send published; dev-6e1e23255 13:35Z: exec both ways 2-3 s, tunnel 7 s, preview 55 s, both sends
  published (60 MB); dev-6e1e23255 15:06Z: M as target: terminal in typed-while-frozen once + live 7.8 s, reattach ok, tunnel 7.1 s, preview
  46 s, L2>M send published. The Mac's own CLI sign-in (device-flow login of 11:33Z) expired at ~15:13Z, so the Mac-as-client parts of that
  last run (terminal out, exec M>L2, send M>L2) failed with "Your Paperboat session is no longer valid"; they had passed at 12:50Z and 13:44Z.

## Product bugs found by these runs and fixed
- c5362743f: after a wake the first preview renewals are refused 403 (host presence not yet online) or 412 (readiness moved the generation); the
  renew loop took that for lease loss and stopped the preview (lost in 3 of 7 runs). Test TestRenewalSurvivesOwnerRefusalsUntilThePresenceReturns,
  CI cli -race green. Plain previews (no --ttl) now serve again 8-78 s after thaw in 8 of 8 runs.
- 6e1e23255: an in-flight `pb send` failed in 2 of 9 runs because the sender's daemon answers 499 canceled when its peer stream dies and the CLI
  treated 499 as final; now retried. Test TestCanceledAnswerFromTheDaemonIsRetried (hermes: go test + vet ./internal/filetransfer green).
  Afterwards 8 of 8 in-flight sends (both roles, three OSes) published.

## Not sleep bugs, logged (failures.md)
- Windows target ignores an Enter typed in the first seconds after `pb connect --session` starts (tty still cooked turns CR into LF); reproduces with no sleep.
- Prod 503 "temporarily unavailable" on preview create / terminal create / availability PUT (REL-F21-168 has the details); the driver retries
  preview/tunnel/terminal creates up to 4 times with 15 s pauses: retry counts in the final passing L, W and M runs: 0 (earlier runs aborted on the first 503 and were rerun).
- `pb exec` to a machine that just went offline returned its clear error within 18-60 s; in 5 of 16 runs it had not returned after 40 s (cut by the harness).
- After `pb install` over a running Linux install the previous generation's session-owner systemd unit stayed active (seen with the watchdog drop-in restart; not investigated).
