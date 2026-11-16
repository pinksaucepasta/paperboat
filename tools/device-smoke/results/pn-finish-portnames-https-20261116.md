# F27-01..05, X-45, X-46: port names, top-level names, front desk, HTTPS front desk, ports in use (PN-FINISH)

Build dev-e37835bfc (main e37835bfc, includes 18de3c2f5 port names, 28f11abc3 HTTPS upstream, cf307cc45 Linux 80/443 fallback) on every machine; `pb --version` printed that version on pnf-h, pnf-v, pnf-late (Linux containers on pbvm2), pnf-w (winpool-2, Windows Server, IIS W3SVC/WAS stopped first) and pnf-m (macpool-2, macOS arm64). All in one account. Nothing configured on any viewer.

## tools/device-smoke/port_names.py (names, listing, machine beats all-machines, numeric label, front desk http, name equal to a machine refused, late machine)
New step `late_machine_takes_address` (`--late-machine`): a third machine is renamed to an existing top-level name; `pb port name list` shows the name shadowed and the viewer reaches that machine's own page, not the named port; renamed back in cleanup.

| host | viewer | result |
|---|---|---|
| pnf-h (L) | pnf-v (L) | PASS, all 9 steps incl. shadow_refused and late_machine_takes_address |
| pnf-h (L) | pnf-w (W) | PASS, same steps |
| pnf-h (L) | pnf-m (M) | PASS, same steps |
| pnf-m (M) | pnf-w (W) | PASS, same steps |
| pnf-m (M) | pnf-v (L) | PASS (no shadow/late steps) |

## Names grant no access (second account), hand run
Name `pnfout` set on pnf-h for a listening port. A machine of the same account (pnf-v) opens https://pnfout.pnf-h.<domain>/who -> `portOUT`. A machine of ANOTHER account (a pbvm2 container paired to a different account, healthy gateway on 127.100.0.1:80/443, the name resolves there) gets `curl: (35) tlsv1 alert internal error` (no certificate or route for a name of another account): the port is not reached. Name unset afterwards. Server-side proof: TestPortNamesOnPostgres (only listed ports' names are delivered).

## tools/device-smoke/https_front_desk.py (F27-04, F27-05)
Linux host with a Traefik-like web server: TLS 0.0.0.0:443 (self-signed) and plain 0.0.0.0:80 that redirects to https://<same host>/, both started before the guard. Ports 80 and 443 were listed with `pb tunnel policy allow` in this first run (automatic listing stopped at 1024 and at the daemon user's own sockets; found when 443/https failed without it: the front-desk wildcard route only exists when the front-desk port is in the machine's listed ports). See the second run below: no allow needed. Steps: web server holds both ports and nft shows DNAT for them (guard moved to free ports); front desk 443/https opens an unmatched name over TLS; `443.<host>` opens; the host opens its own address; front desk 80/http gets the explanation (502, X-Paperboat-Error origin_redirect_loop), not a loop.
- host pnf-h (L) viewers pnf-v (L), pnf-w (W), pnf-m (M): PASS, all 5 steps.
- `--high-ports` (free ports >= 1024, no sudo, macOS too; skips the 443 label, guard, policy and nft steps because only numeric label 443 is https): host pnf-m (M), viewers pnf-w (W) and pnf-v (L): PASS, 4 steps.
- Uninstall on pnf-h with the web server still holding 80/443: 2 DNAT rules before, `nft list tables` empty after, 0 paperboat units.

## pb doctor names a held 80/443 on macOS and Windows
- macOS (pnf-m): python holding 0.0.0.0:80/443, daemon restarted: doctor `degraded`, "Browser URLs are off because another program is using port 80/443"; after the program stopped and a restart: `healthy`.
- Windows (pnf-w): W3SVC/WAS started, daemon restarted: doctor `degraded`, "... (Windows HTTP service (HTTP.sys, for example IIS), PID 4)"; after W3SVC/WAS stopped and a restart: `healthy`.

## Console
CONSOLE-2 verified the Network "Port names" add/remove and the machine "Front desk" live on a376d89b3.

## Gaps
- Windows as the HOST of the scripted checks: both scripts run on a Unix host (python3 servers, os.uname, nft). A Windows host naming ports was not run.

## Second run: default private access covers 80 and 443 of any owner (dev-4188ece01, server .29, migration 331)
User decision: 80, 443 and 1024+ are open by default for everyone already allowed; other low ports need an explicit allow. Change: shared/machineservices.AutomaticPort (client discovery filter, server snapshot check and list path, SQL machine_service_authorities, shared/policy default), and discovery lists listeners on 80 and 443 whoever owns them (Linux sock_diag and /proc, macOS netstat, Windows TCP table; other low ports stay owner-only). `pb --version` dev-4188ece01 on pnf-h, pnf-v, pnf-late (Linux containers on hp), pnf-w (winpool, Windows Server), pnf-m (macpool, macOS arm64). The web server runs as root (sudo), like Coolify's Traefik, and no `pb tunnel policy allow` is made.
- https_front_desk.py, host pnf-h (L): viewers pnf-v (L), pnf-w (W), pnf-m (M): PASS, all 5 steps each.
- https_front_desk.py, host pnf-m (M, root-owned python on 80/443): viewers pnf-v (L) and pnf-w (W): PASS, all 5 steps each.
- port_names.py, host pnf-h (L): viewers pnf-v, pnf-w, pnf-m: PASS, all 9 steps each (incl. shadow_refused and late_machine_takes_address).
- Before the discovery fix, with the web server owned by root, the same check failed at front_desk_https (TLS alert on the viewer: no wildcard route because 443 was not listed); with the web server run as the daemon's user it passed.

## Gaps (second run)
- Windows as the HOST: another lane (PN-FINISH-W) runs it; not claimed here. Windows as viewer passes.
