# X-42 bugreport part by hand on the fleet (lane X), 2026-11-13 ~02:00 IST

Build: `pb version` = dev-3ef4b7b39 (includes 57e4ed4cb, the daemon writes status.json through bugreportStatus) on lanex2-l (Linux container), lanex2-m (the real apple Mac), lanex2-w (Windows runner winpool-3). Doctor healthy on all three.

## status.json is trimmed (`pb bugreport --json` on each machine, bundle opened with python zipfile)
| machine | bundle | status.json | other machine names (the other two fleet machines, rel-x-p, apple/Final) | host keys / ssh user / home directory / owner / platform / peers list |
|---|---|---|---|---|
| lanex2-l | bugreport-support_fab3582c-....zip, 1346748 B | 3195 B; keys daemon_state, daemon_version, generation, health, home_relay, machines, observed_at, schema, ssh_domain | none | none (the word "home" is the key home_relay) |
| lanex2-m | made on the Mac | 3167 B, same keys | none | none |
| lanex2-w | made on the Windows runner | 3083 B, same keys minus home_relay | none | none |
`machines` holds 6 entries on each side with only status fields (health, eligible, generation, readiness, NAT mapping, path); `id` and `alias` are empty strings for every entry, so no other machine is named. Before 57e4ed4cb the same file held 52 peers with id, name, owner, platform, SSH user, host keys and home directory (FIX-X on fixx-a).

## A bundle made on one OS opens on the others (6 directions)
Each bundle was copied to the other two machines and opened there with python zipfile (testzip, every file decoded as UTF-8): L>M, L>W, M>L, M>W, W>L, W>M all print `opened: 4 files, zip test ok, text files 4, with CRLF 0, non-ASCII names 0, secret-looking names none` (files: manifest, recent_events, redacted_events, status).
Not covered: long archive names and Unicode names (the bundles hold none), CRLF logs (no CRLF in these bundles), and "its support reference resolves" (not looked up).
