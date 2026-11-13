# X-15 Windows parts, second pass, 2026-11-13 09:20-09:48 IST, build dev-051c1f7f2 (current main) on lanex2-l, lanex2-m, lanex2-w (new winpool-6 runner)

`pb version` of all three: dev-051c1f7f2.

## L<->W (scripted, tools/device-smoke/xplat_send.py --devices L,W --only batch,modes,refuse,inboxset; xplat-send-20261113T041656Z.json, 8/8 PASS)
batch L>W/W>L (ten files equal sha256, eleven refused naming the limit of 10), modes L>W/W>L (no setuid/group/other/exec from the sender; Windows receiver owner-only ACL), folder+symlink refused both ways, `pb inbox set` both ways (relative and other-OS form refused naming it, ~, spaces/non-ASCII, D:\ and \\localhost\c$ share accepted and each delivered).

## Resume, by hand (60 MiB random file made on the sender, `pb send`, 4 s later `pb service restart` on the receiver; sha256 compared; script = resume_hand.py logic of xplat_send.t_resume with the file made on the sender)
W>L: restart rc 0, send rc 0 after 40 s, equal sha256 (Inbox file removed)
L>W: restart rc 0, send rc 0 after 39 s, equal sha256
M>W: restart rc 0, send rc 0 after 105 s, equal sha256
W>M: restart rc 0, send rc 0 after 47 s, equal sha256
The earlier "W>L resume does not finish" was the harness: t_resume uploads the 60 MiB file to a Windows sender through ssh (171 kB after 4 min); it is not a product stall. The Haiku lead (no deadline on the first renewal) was not reproduced and the code path it names does not involve the receiver; no code change.

## Not finished
M<->W scripted parts on dev-051c1f7f2: the Windows runner winpool-6 died at 09:47 IST (ssh tunnel 22206 refused) 87 s into M>W batch; reruns failed on the dead tunnel. Team file request to/from W and the OneDrive/redirected-folder path are not run yet.
