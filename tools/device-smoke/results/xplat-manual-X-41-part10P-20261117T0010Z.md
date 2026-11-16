# X-41 part 10P (R-X-41-P): Linux uninstall seen from the Mac, rerun with 7ae7f56a5 (scope-refusal refresh) and 4b75e3ac1 (cert cache by machine id)

Machines: rx41p-l = Linux container on pbvm2 (laneh-ubuntu24-systemd + openssh-server, user pbt), rx41p-m = macpool-3; one fresh account (fresh_account.mjs).
Both `pb version` dev-65dddfdfe (contains 7ae7f56a5 and 4b75e3ac1).

## uninstall:L observed from M: PASS twice
- xplat-removal-20261116T182538Z.json: before: M exec + `pb ssh` L; L `pb uninstall`; M lists L gone, exec says "No machine has that name or ID", alias removed, M 2 events/min; L installed + paired again: new id and key, M exec reachable 5 s after the new identity was listed, `pb ssh` 5 s later.
- xplat-removal-20261116T183946Z.json: same flow again: M 3 events/min, exec 4 s, `pb ssh` 5 s.
- Hold between the two runs (xplat-removal-uninstallL-hold-20261116T1826Z.out): M -> L `pb exec` and `pb ssh` once a minute for 10 min after the reinstall: 10/10 rc 0. Part 8's failure (M -> L exec lost for 49 min after L installed again) does not reproduce.

## Not product failures (no result kept)
- 18:16Z: the container had no sshd, so `pb ssh` to L had no host keys before the removal (fixture; openssh-server installed).
- 18:20Z: the shared build directory dev-2e3cad1f5 was pruned while the flow was running (binaries now copied to a private directory).
