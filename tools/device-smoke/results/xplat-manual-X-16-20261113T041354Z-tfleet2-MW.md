# T-FLEET2: X-16 M<->W terminal, three clean rounds on dev-051c1f7f2, 2026-11-13 09:20-09:43 IST

lanex2-m (pbmac VM, macOS) and lanex2-w (winpool-6, Windows): `pb version` dev-051c1f7f2 on both and on lanex2-l, doctor healthy on M and W, exec ok in all six directions at 09:19 IST.
Driver: tools/device-smoke/xplat_term.py --devices M,W (a real pty through an ssh -tt hop into the client, `pb connect <target>`; the client's own terminal handling is tested, the target's default shell answers). Three rounds in a row, each round runs the seven X-16 checks in both directions (M>W target cmd.exe, W>M target zsh): shell starts and answers, UTF-8 and wide characters echoed unchanged, window resize reaches the remote PTY, Ctrl-C stops a long command and the shell answers again, Ctrl-D behaves (cmd stays usable, unix shell exits and pb connect exits 0), the remote exit code (3) is pb connect's exit status, and after the client vanished a reattach replays the earlier output of a session that kept running.

Raw PASS/FAIL lines of the three rounds (the result JSONs are not committed: failure details of the pexpect driver carry its login command line):

=== round 1 09:20:27
PASS X-16:shell:M>W 25s the target shell started and answered (cmd)
PASS X-16:utf8:M>W 8s UTF-8 and wide characters typed and echoed unchanged (cmd)
PASS X-16:resize:M>W 10s a window resize reached the remote PTY (45x132, cmd)
PASS X-16:ctrlc:M>W 31s Ctrl-C stopped a long command and the shell answered again (cmd)
PASS X-16:ctrld:M>W 37s Ctrl-D did not end the cmd session, which stays usable like a local one
PASS X-16:exit:M>W 25s the remote exit code (3) is the exit status of pb connect (cmd)
PASS X-16:replay:M>W 23s after the client vanished, a reattach replayed the earlier output of a session that kept running (cmd)
FAIL X-11:connect:M>W 85s: timed out waiting for 'H:XP_1_PLAIN:<marker>'; screen tail: '9;9;C:\\Users\\runneradmin\x07\r\nC:\\Users\\runneradmin
PASS X-16:shell:W>M 10s the target shell started and answered (unix)
PASS X-16:utf8:W>M 11s UTF-8 and wide characters typed and echoed unchanged (unix)
PASS X-16:resize:W>M 14s a window resize reached the remote PTY (45x132, unix)
PASS X-16:ctrlc:W>M 16s Ctrl-C stopped a long command and the shell answered again (unix)
PASS X-16:ctrld:W>M 12s Ctrl-D at an empty prompt ended the unix shell and pb connect exited 0
PASS X-16:exit:W>M 11s the remote exit code (3) is the exit status of pb connect (unix)
PASS X-16:replay:W>M 30s after the client vanished, a reattach replayed the earlier output of a session that kept running (unix)
FAIL X-11:connect:W>M 69s: timed out waiting for 'H:XP_1_PLAIN:<marker>'; screen tail: 'connected  d\x1b[5;36H\x1b[?25h\x1b[27m\x1b[?25l\x1b[7m\
FAIL /home/ubuntu/paperboat/tools/device-smoke/results/xplat-term-20261113T035741Z.json
results/xplat-term-20261113T035741Z.json
=== round 2 09:27:41
PASS X-16:shell:M>W 25s the target shell started and answered (cmd)
PASS X-16:utf8:M>W 25s UTF-8 and wide characters typed and echoed unchanged (cmd)
PASS X-16:resize:M>W 28s a window resize reached the remote PTY (45x132, cmd)
PASS X-16:ctrlc:M>W 31s Ctrl-C stopped a long command and the shell answered again (cmd)
PASS X-16:ctrld:M>W 38s Ctrl-D did not end the cmd session, which stays usable like a local one
PASS X-16:exit:M>W 26s the remote exit code (3) is the exit status of pb connect (cmd)
PASS X-16:replay:M>W 41s after the client vanished, a reattach replayed the earlier output of a session that kept running (cmd)
FAIL X-11:connect:M>W 85s: timed out waiting for 'H:XP_1_PLAIN:<marker>'; screen tail: 'Administrator: C:\\Windows\\System32\\cmd.exe - python  
PASS X-16:shell:W>M 12s the target shell started and answered (unix)
PASS X-16:utf8:W>M 11s UTF-8 and wide characters typed and echoed unchanged (unix)
PASS X-16:resize:W>M 13s a window resize reached the remote PTY (45x132, unix)
PASS X-16:ctrlc:W>M 15s Ctrl-C stopped a long command and the shell answered again (unix)
PASS X-16:ctrld:W>M 10s Ctrl-D at an empty prompt ended the unix shell and pb connect exited 0
PASS X-16:exit:W>M 10s the remote exit code (3) is the exit status of pb connect (unix)
PASS X-16:replay:W>M 29s after the client vanished, a reattach replayed the earlier output of a session that kept running (unix)
FAIL X-11:connect:W>M 70s: timed out waiting for 'H:XP_1_PLAIN:<marker>'; screen tail: '     connected  d\x1b[7;36H\x1b[?25h\x1b[?25l\x1b[27m\x1
FAIL /home/ubuntu/paperboat/tools/device-smoke/results/xplat-term-20261113T040549Z.json
results/xplat-term-20261113T040549Z.json
=== round 3 09:35:49
PASS X-16:shell:M>W 25s the target shell started and answered (cmd)
PASS X-16:utf8:M>W 26s UTF-8 and wide characters typed and echoed unchanged (cmd)
PASS X-16:resize:M>W 28s a window resize reached the remote PTY (45x132, cmd)
PASS X-16:ctrlc:M>W 18s Ctrl-C stopped a long command and the shell answered again (cmd)
PASS X-16:ctrld:M>W 20s Ctrl-D did not end the cmd session, which stays usable like a local one
PASS X-16:exit:M>W 25s the remote exit code (3) is the exit status of pb connect (cmd)
PASS X-16:replay:M>W 41s after the client vanished, a reattach replayed the earlier output of a session that kept running (cmd)
FAIL X-11:connect:M>W 85s: timed out waiting for 'H:XP_1_PLAIN:<marker>'; screen tail: '9;9;C:\\Users\\runneradmin\x07\r\nC:\\Users\\runneradmin
PASS X-16:shell:W>M 11s the target shell started and answered (unix)
PASS X-16:utf8:W>M 11s UTF-8 and wide characters typed and echoed unchanged (unix)
PASS X-16:resize:W>M 16s a window resize reached the remote PTY (45x132, unix)
PASS X-16:ctrlc:W>M 16s Ctrl-C stopped a long command and the shell answered again (unix)
PASS X-16:ctrld:W>M 11s Ctrl-D at an empty prompt ended the unix shell and pb connect exited 0
PASS X-16:exit:W>M 11s the remote exit code (3) is the exit status of pb connect (unix)
PASS X-16:replay:W>M 31s after the client vanished, a reattach replayed the earlier output of a session that kept running (unix)
FAIL X-11:connect:W>M 70s: timed out waiting for 'H:XP_1_PLAIN:<marker>'; screen tail: 'connected  d\x1b[7;36H\x1b[?25h\x1b[27m\x1b[?25l\x1b[7m\
FAIL /home/ubuntu/paperboat/tools/device-smoke/results/xplat-term-20261113T041333Z.json
results/xplat-term-20261113T041333Z.json

## Result
X-16 M>W and W>M: 14 of 14 in each of the three rounds (42 of 42 per direction pair), no reconnect, no early exit.
The only FAIL lines are `X-11:connect:*` (the ENV delivery leg of row X-11: the expected ENV marker never reached the screen, both directions, all three rounds); that is another row and is not part of X-16.
History kept for honesty: on cycle 2 (dev-e4d2cc0f8, 08:21-08:43 IST) one of three rounds failed `X-16:utf8:M>W`: `pb connect` from the Mac to W ended about 3 s after start, the Mac diagnostics showed `control_request canceled` then `command_failed` with a canceled net operation. Code reading (cli/cmd/pb/main.go:48-60) says the command context is only cancelled by SIGINT/SIGTERM before the session exists, and a cancelled net error is not recognised by the classifier (cli/cmd/pb/failure_classify.go identity switch), so the user sees the generic message. Not reproduced in these three rounds or in the earlier two clean rounds; the canceller was not identified.
