# SSH certificates on the fleet Mac (2026-11-16, server .31, dev-caf37d9c6 on lanex2-l2/-m/-w)

Run of mac.sh under shared fleet-mac.lock + pbmac.lock. Before it, the Mac's certificate for lanex2-w issued to its earlier API-token sign-in was removed (W's KRL refused it; fixed in 2c7623fc4).

```
removed
== versions
  ▄▄▝▀▀▀▀▀▘▄▄   Version dev-caf37d9c6
  ▄▄▝▀▀▀▀▀▘▄▄   Version dev-caf37d9c6
== Mac host CA line
cert-authority,principals="pb:machine_9e1851b7-5763-40ab-9834-720becc3
== L2 host CA line
cert-authority,principals="pb:machine_8139eee2-c661-484a-b1de-43be3f25
== L2 -> M
src=ea52db4c5ef0890f
pb ssh: admin
plain ssh: admin
 Valid: from 2026-11-16T12:06:02 to 2026-11-17T00:07:02
 admin
 pb:machine_9e1851b7-5763-40ab-9834-720becc3eb6d
scp=ea52db4c5ef0890f
sftp=ea52db4c5ef0890f
rsync=ea52db4c5ef0890f
== M -> W (lanex2-w, Windows target)
src=61cd903d5d15cb6f
pb ssh: victus\pujan
plain ssh: victus\pujan
 Valid: from 2026-11-16T12:38:23 to 2026-11-17T00:39:23
 pb:machine_3a405daa-4c1e-48a2-aeef-a6a52e3065f1
scp=61cd903d5d15cb6f
sftp=61cd903d5d15cb6f
== M -> L2
src=b384994f043531f8
pb ssh: tester
plain ssh: tester
scp=b384994f043531f8
sftp=b384994f043531f8
rsync=b384994f043531f8
```
