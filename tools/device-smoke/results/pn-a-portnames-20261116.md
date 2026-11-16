# Port names, front desk, top-level names: hand run, dev-18de3c2f5 (main 18de3c2f5), server with migrations 309+310

All machines in one account. `pb version` on every machine: dev-18de3c2f5.
- L host pna-l: Ubuntu 24.04 amd64 container on pbvm2 (machine_9501a163-481d-4041-a29e-42840b57fcbc). python3 http.server on 127.0.0.1:8081 (/who = port8081) and :8082 (/who = port8082).
- L viewer pna-v: Ubuntu 24.04 container on pbvm2 (machine_9e3bd34f-c17c-4ada-b5f0-9f1724dd4016). Nothing configured.
- M viewer pna-m: macpool-7 (GitHub macOS arm64 runner; machine_29155ead-e3f0-4788-8b79-092635fa9bcb). Nothing configured.
- W viewer pna-w: winpool-2 (GitHub Windows Server runner; machine_1b8b931c-db17-4d50-8395-8b932974248b). Nothing configured. The runner's IIS (W3SVC, HTTP.sys `http://*:80/`) held port 80: pb doctor named it ("Browser URLs are off because another program is using port 80/443 (Windows HTTP service (HTTP.sys, for example IIS), PID 4)"). After W3SVC/WAS were stopped, pb turned browser URLs on by itself.
- Late machine pnajelly: a third container on pbvm2 (machine_2e3e7870-10f8-4307-ad14-35748655c76c).

Set from pna-l (or from pna-v for the front desk):
- `pb port name set app 8081 --machine pna-l`
- `pb port front-desk pna-l 8081 http`, set from the viewer pna-v
- `pb port name set pnajelly 8081 --top-level --machine pna-l`; `pb port name set pna-v 8081 --top-level --machine pna-l` is refused with name_is_machine
- `pb port name set pnatop 8081 --top-level --machine pna-l`
- `pb port name set pnaall 8081 --all-machines`; `pb port name set pnaall 8082 --machine pna-l`; `pb port name set other 8081 --all-machines`

Opened over HTTPS with normal certificate checks (curl on L/M, Invoke-WebRequest on W):

| address | L pna-v | M pna-m | W pna-w |
|---|---|---|---|
| app.pna-l.ppr.boats | 200, verify 0 | 200, verify 0 | 200 |
| coolify.pna-l.ppr.boats (no name: front desk 8081) | 200 (before the front desk was set: TLS refused, nothing on 80) | 200 | 200 |
| pnajelly.ppr.boats (top-level, before the machine was added) | 200 | - | - |
| pnaall.pna-l.ppr.boats /who (machine name beats all-machines) | port8082 | port8082 | port8082 |
| other.pna-l.ppr.boats /who (all-machines) | port8081 | port8081 | port8081 |
| pnatop.ppr.boats /who (top-level) | port8081 | port8081 | port8081 |
| 8081.pna-l.ppr.boats /who (numeric label) | port8081 | port8081 | port8081 |

The pnajelly machine was added after the top-level name:
- `pb port name list` shows `pnajelly.<domain> 8081 top_level shadowed: machine pnajelly has this name and wins`.
- pnajelly.ppr.boats opens that machine's own page on L, M and W ("pnajelly — Paperboat", listing its port 38693), not port 8081 of pna-l.
- `pb exec pnajelly -- hostname` answers.

Unset:
- `pb port name unset pnatop --top-level`: pnatop.ppr.boats is no longer served (TLS refused).
- `pb port name unset pnaall --machine pna-l`: pnaall.pna-l falls back to the all-machines name (port8081).

Not checked on devices:
- The console Network page (W-15, console lane).
- "Names grant no access" with a second account. Covered by TestPortNamesOnPostgres: only listed ports' names are delivered.
- An https front desk. The scheme reaches the route (TestCoordinatorRoutesServerPortNamesFrontDeskAndTopLevelNames); the TLS upstream itself is F27-04 (PN-B).

## Automated check: tools/device-smoke/port_names.py (host pna-l, dev-18de3c2f5 on every machine)

Run on pna-l as `python3 port_names.py <viewer> --shadow-machine pnajelly`.
- viewer pna-v (L): PASS
- viewer pna-m (M): PASS
- viewer pna-w (W): PASS

Steps, all PASS in each run: set_names, viewer_machine_name, viewer_machine_beats_all_machines, viewer_top_level, viewer_port_number, viewer_front_desk, list, shadow_refused, cleanup. enrollment_unchanged is true in each run.

The first run (viewer pna-v) failed only at cleanup, because the script passed `--json` to `pb port name unset`. The script was fixed and the leftovers were removed by hand; then the three runs above.
