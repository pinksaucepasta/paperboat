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

## Windows host, Mac viewer (dev-a46d68d22, server .30, clean macpool-8): PASS
| script | result |
|---|---|
| port_names.py macpool-pnfw-8 --shadow-machine pnfw-hv --late-machine pnfw-hv | PASS, all 9 steps (pnC) |
| https_front_desk.py macpool-pnfw-8 (web server holding 80/443) | PASS: front desk https, 443 label, own address, redirect explanation (fdC) |
Earlier on dev-729ce3c0e (macpool-2) the same checks passed too. Runs on macpool-3 (dev-f3f5481be) failed on the first fetch because that runner
carried another lane's pb and account; released and revoked, they do not count.

## Found: policy changes reach the host only with its next heartbeat (fixed in da5f27961)
`pb tunnel policy allow <machine> 81` (a port below 1024) to the port opening on a viewer, measured with a host-side origin on 127.0.0.1:81:
| host | viewer | server .28, dev-729ce3c0e | server .29, dev-f3f5481be |
|---|---|---|---|
| Linux (pnfw-hv) | Mac | 17 s | 28 s |
| Windows | Linux | 91 s | 41 s |
| Mac | Linux | 178 s | 86 s |
Cause: lazyaccess upsert/delete and machineservices.UpdatePolicy changed what the host reads in the reply to its next observation but published no machine topic, so the host waited for its jittered 4-minute heartbeat. Fix: they publish `machine_services` for the machine in their transaction (TestPolicyPostgres, TestMachineServicePostgresAuthorityLifecycle, CI run 43814899163). Server .30 (push fix) cut it to 6-16 s (Linux 10-15, Windows 6-16): the observation a policy wake sends carries the old policy, and the port the policy lists was announced
only at the next port-watch tick (2 s Linux, 5 s Windows, 10 s macOS). Fix 2decdc2f2 (cli, TestRuntimeObservationAnnouncesAPortAPolicyListsRightAfterTheWake): the loop re-reads the services after a wake and sends again at once.
Measured on dev-a46d68d22 on all three hosts (allow -> open on a viewer, probe every 1-4 s, 2-3 runs; the first run of a pair is warm-up):
| host | viewer | seconds |
|---|---|---|
| Linux (pnfw-hv) | Windows | 9, 6, 5 |
| Linux | Mac | 5, 5 |
| Windows | Linux | 13, 4, 5 |
| Mac (macpool-8) | Linux | 4, 4 |

## Notes
- Windows port discovery polls every 5 s (machine_services_watch_windows.go), Linux 2 s, macOS 10 s: not the cause.
- A Windows daemon restart before these checks made names take about 2 minutes (the daemon re-registers); the final script does not restart the daemon.
