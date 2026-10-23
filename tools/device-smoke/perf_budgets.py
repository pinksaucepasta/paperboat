"""Data-plane performance budgets per selected path, checked by perf_dataplane.py (PLAN 3b).

Each entry is metric: (kind, bound) where kind 'max' fails above the bound and 'min' below it.
A path without an entry fails the check; the 'cli' entry applies to every path. Bounds come from the first two device runs (hp,
two containers on one Docker bridge, dev-f9e28273, 2026-10-23: exec median 4.8 / 2.9 s, max
12.6 / 3.9 s, send 57 / 68 Mbit/s, scp 61 / 40 Mbit/s, rsync 70 / 91 small files/s) with
headroom for noise; they catch regressions. Exec latency is dominated by per-operation access sessions (PLAN 3b design note).
"""
BUDGETS = {
    # perf_idle.py: one machine, 10 min idle, all pb processes (PR-03). Round 16 dev-3f068585 on a
    # pbvm2 container measured 3.4% CPU, 417 wakeups/s and 6.6 MB/h on TCP 443 (control plane and
    # edge); before the idle fixes 17-20%, ~1,680/s and 147-172 MB/h.
    'idle': {
        'cpu_percent': ('max', 5),
        'wakeups_per_s': ('max', 600),
        'tls_mb_per_hour': ('max', 15),
    },
    # Checked on every path: local commands answer from the binary and the daemon snapshot
    # (PR-07; hp: --version 32 ms, status 960 ms before it stopped listing machines on the server).
    'cli': {
        'cli_start_ms': ('max', 50),
        'status_ms': ('max', 300),
    },
    'direct': {
        'exec_true_median_ms': ('max', 7000),
        'exec_true_max_ms': ('max', 20000),
        'send_mbit_s': ('min', 30),
        'scp_mbit_s': ('min', 25),
        'rsync_small_files_s': ('min', 35),
    },
}
