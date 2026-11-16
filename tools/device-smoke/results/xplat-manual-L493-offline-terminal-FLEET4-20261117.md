# L493 offline terminal session registered by the host once it is online (lane FLEET4, 2026-11-17 04:12 IST)

Build dev-b112b2a4a (host adoption b112b2a4a; client retry f1bd427a4) on two Linux containers on pbvm2, `fos-a` (client) and `fos-b` (host), paired to one account; server 2026.11.13.49 (healthy 5 min). Steps as in failures.md "L493 DEVICE CHECK".

```
1. API cut (iptables, one address) on fos-a and fos-b: `pb doctor` on A "degraded", authentication unavailable
2. pb connect fos-b new, typed `echo hi_$((6*7)); sleep 600`: shell answered (offline admission); connect then killed
   bash -l on fos-b: 1; `pb session list fos-b` from the account's other online machine: not listed yet
3. paperboat user units on fos-b restarted: bash -l still 1
4. fos-b unblocked, fos-a still blocked: session listed from the other machine after 8 s: terminal_offline_4780df57_...
5. pb session close fos-b <id> (confirm code): "Session closed. Recent output was deleted."; bash -l on fos-b after 15 s: 0
```
PASS (steps 4 and 5), run three times on dev-b112b2a4a (04:12, 04:17, 04:21 IST), listed after 8 s each time.

Queue file (read in the third run, 22:50 UTC): `/home/pbt/.local/state/paperboat/runtime/carrier-0-offline-audit.json.adopt` (the carrier names it `<role>-offline-audit.json.adopt`, not `runtime/offline-audit.json.adopt` as the failures.md steps say). After the host service restart, with the API still cut, it held the `offline_session_started` event of the session; after the host's API returned and the session was listed it read `null` (drained). Fixtures revoked by exact id, containers removed.
