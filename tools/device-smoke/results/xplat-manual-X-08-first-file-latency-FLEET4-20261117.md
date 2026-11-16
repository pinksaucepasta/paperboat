# X-08 first file after a fresh assignment: latency before/after aa2b77e02 (lane FLEET4, 2026-11-17 03:19-03:22 IST)

Fleet build dev-479e6f8a9 (contains aa2b77e02, before 8200fbeb1; server .48 with migration 374) on lanex2-l2 (L) and lanex2-m (M, the pbmac VM). Same scratch check as a1f2f3b4c (fresh repository + fresh assignment per cycle, a 300-byte file written on L, pass = byte-identical in the repository and on M). Variant A: written right after L's assignment, before M applies. Variant B: written after both machines are assigned (the case this recheck is about).

```
03:19:45 cycle 0 (A): PASS repo +7s, M +11s
03:20:19 cycle 1 (B): PASS repo +4s, M +9s
03:20:49 cycle 2 (A): PASS repo +7s, M +11s
03:21:25 cycle 3 (B): PASS repo +4s, M +9s
03:22:01 cycle 4 (A): PASS repo +8s, M +12s
03:22:39 cycle 5 (B): PASS repo +4s, M +9s
FIRST08 PASS: 6 cycles, 0 failed
```

| variant B (both assigned) | before (dev-c7ed1040f, 8 cycles) | after (dev-479e6f8a9, 3 cycles) |
|---|---|---|
| file in the repository | 59-60 s | 4 s |
| file on M | 64-75 s | 9 s |

Variant A is unchanged (repo 7-8 s, M 11-12 s).
