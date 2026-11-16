# F27-01..04 W, X-45 / X-46 L<->W: Windows as the HOST of port_names.py and https_front_desk.py (PN-FINISH-W)

Builds: dev-729ce3c0e (first runs, server .28) and dev-f3f5481be (final runs, server .29, main ab37447ae paths unchanged for these checks) on every machine.
`pb --version` printed that version on winpool-9 (Windows Server 2025 runner, IIS W3SVC/WAS stopped), pnfw-hv (Linux, container on hp) and each Mac runner.
Windows host mode: `PB_SMOKE_WINDOWS_PB` set, the scripts run on the controller, the origin servers run on the Windows device with python (tools/device-smoke/winorigin.py).

## Windows host, Linux viewer (dev-f3f5481be, server .29): PASS
| script | result |
|---|---|
| port_names.py pnfw-hv --shadow-machine <mac> --late-machine <mac> | PASS, all 9 steps: set_names, machine name, machine beats all-machines, top-level, port number, front desk http, list, shadow_refused, late_machine_takes_address (+ cleanup) (pnA-pnfw-hv) |
| https_front_desk.py pnfw-hv (web server holding 0.0.0.0:80/443, IIS stopped) | PASS: front_desk_https, port_443_label, host_opens_own_address, redirect_gets_explanation |
| https_front_desk.py pnfw-hv --high-ports (dev-729ce3c0e) | PASS |

## Windows host, Mac viewer
- dev-729ce3c0e, macpool-2: port_names.py all steps PASS except late machine (the third machine, on a VM that was down, was offline); https_front_desk.py PASS in both modes (80/443 and --high-ports).
- dev-f3f5481be, macpool-3: FAIL on the first fetch in 4 runs: that runner turned out to carry another lane's pb and account (its `pb machine list` showed machines of a second account), so those runs do not count. Released and revoked; M<->W stays [~] until a clean Mac runner passes.

## Found: policy changes reach the host only with its next heartbeat (fixed in da5f27961)
`pb tunnel policy allow <machine> 81` (a port below 1024) to the port opening on a viewer, measured with a host-side origin on 127.0.0.1:81:
| host | viewer | server .28, dev-729ce3c0e | server .29, dev-f3f5481be |
|---|---|---|---|
| Linux (pnfw-hv) | Mac | 17 s | 28 s |
| Windows | Linux | 91 s | 41 s |
| Mac | Linux | 178 s | 86 s |
Cause: lazyaccess upsert/delete and machineservices.UpdatePolicy changed what the host reads in the reply to its next observation but published no machine topic, so the host waited for its jittered 4-minute heartbeat. Fix: they publish `machine_services` for the machine in their transaction (TestPolicyPostgres, TestMachineServicePostgresAuthorityLifecycle, CI run 43814899163). Re-measurement after the deploy: see the Log.

## Notes
- Windows port discovery polls every 5 s (machine_services_watch_windows.go), Linux 2 s, macOS 10 s: not the cause.
- A Windows daemon restart before these checks made names take about 2 minutes (the daemon re-registers); the final script does not restart the daemon.
