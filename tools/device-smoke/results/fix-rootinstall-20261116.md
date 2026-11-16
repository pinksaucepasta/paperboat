# FIX-ROOTINSTALL: pb install as root on Linux (2026-11-16)

Machines: throwaway privileged systemd containers on hp, every command as root (uid 0).
- Ubuntu 24.04 (`pbtest-systemd` image, has dbus/logind)
- Debian 12 (`frt-debian12-systemd`: systemd + ca-certificates only; no dbus, no libpam-systemd, no /run/user)
Peer for exec: hp's own pb (dev-64faf9466) and the other root container. Pairing from a personal recipe
(`frtroot`: tags frt, `[[reach]] dst tag:frt allow exec`) with `PB_RECIPE=... pb pair` and stdin closed.

## Before the fix (dev-a46d68d22, main)
Ubuntu root `pb install`: `activate replacement service: exit status 1` (systemctl --user, no user manager).

## After
| Step | Ubuntu 24.04 root | Debian 12 root, no dbus |
|---|---|---|
| `pb install` | PASS dev-593e40a64, dev-a1db4af6a: `paperboatd.service` active in /etc/systemd/system, no ~/.config/systemd/user | PASS dev-593e40a64, dev-5081bf440, dev-a1db4af6a |
| `pb pair` from recipe, no prompt | PASS (frt-l, frt-l2, frt-l3) | dev-593e40a64 FAIL `apply service declaration` (privileged host service: keep-awake, systemd-inhibit "Failed to connect to bus") -> fixed by "Pairing on a Linux host without D-Bus no longer fails at keep-awake"; PASS dev-5081bf440, dev-a1db4af6a (frt-d, frt-d2) |
| exec both ways | PASS hp<->frt-l (dev-593e40a64, dev-72ce5da1e); frt-l3<->frt-d2 root<->root `id -u`=0 (dev-a1db4af6a) | PASS hp<->frt-d (dev-5081bf440, dev-a1db4af6a); frt-d2->frt-l3 |
| `pb update --from` in place | PASS dev-593e40a64 -> dev-72ce5da1e, one generation left, exec both ways after | PASS dev-5081bf440 -> dev-a1db4af6a, one generation left, exec both ways after (first hp->frt-d exec in the first seconds after the update: "could not reach", next 2 PASS) |
| `pb status` / `pb doctor` | browser certificate step retried forever as root -> fixed by "Browser URLs work for a machine enrolled as root on Linux"; clean on dev-a1db4af6a | logout_survival "Paperboat runs as a system service"; browser warning gone on dev-a1db4af6a |
| `pb uninstall` | PASS: no unit, file, process, nft table or /root/.pki left; machine record removed | PASS (also after the failed pair) |

Leftover check (`leftovers.sh`): systemctl units, /etc/systemd/system, /usr/local/libexec/paperboat,
/var/lib/paperboat*, /run/paperboat*, ~/.local/bin/pb, ~/.local/state/paperboat, ~/.config/paperboat,
/tmp/paperboat-0, pb processes, nft tables: all empty after every uninstall.

Notes: a recipe session sees only the recipe's workspace (a team machine is refused, by design); the
recipe apply "server denied access" seen at first cleared server-side during the run (revision 1, 2 applied).
