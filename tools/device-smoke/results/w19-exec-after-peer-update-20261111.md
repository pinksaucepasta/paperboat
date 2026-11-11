# W19: first pb exec while the peer updates in place (2026-11-11)

Builds: old CLI dev-ea326e98da (= origin ea326e98d). Fixed CLI dev-dcab60a25e (commit dcab60a25 before rebase; window 90 s). dev-19164e4c2c = first cut with a 40 s window.
Each loop runs `pb exec <machine> -- hostname` every ~2 s. Old and fixed CLI run at the same time against the same target and the same in-place `pb install` of a newer dev build.

## 1. hp (Linux) -> winpool-8 (Windows, machine_5b2764df), pb install over the paired install

### 1a. Install B over A (old CLI only); Windows SCM shows a worker gap of 05:06:11 stop -> 05:06:46.9 Hostd running
```
05:05:44 dur=2.9 runnervmm7ud1 
05:05:49 dur=2.6 runnervmm7ud1 
05:05:54 dur=3.0 runnervmm7ud1 
05:05:59 dur=3.0 runnervmm7ud1 
05:06:04 dur=31.2 pb: The local Paperboat service could not complete the request (step: exec_start_uncertain). Check `pb status` for the local service and the t
05:06:37 dur=13.5 runnervmm7ud1 
05:06:53 dur=3.1 runnervmm7ud1 
05:06:58 dur=3.1 runnervmm7ud1 
05:07:03 dur=3.0 runnervmm7ud1 
05:07:08 dur=3.0 runnervmm7ud1 
05:07:13 dur=3.1 runnervmm7ud1 
05:07:19 dur=3.1 runnervmm7ud1 
05:07:24 dur=2.6 runnervmm7ud1 
05:07:29 dur=3.1 runnervmm7ud1 
05:07:34 dur=3.1 runnervmm7ud1 
05:07:39 dur=3.1 runnervmm7ud1 
05:07:44 dur=3.0 runnervmm7ud1 
05:07:49 dur=3.0 runnervmm7ud1 
05:07:54 dur=2.9 runnervmm7ud1 
05:07:59 dur=3.2 runnervmm7ud1 
05:08:04 dur=3.1 runnervmm7ud1 
05:08:10 dur=2.9 runnervmm7ud1 
05:08:15 dur=3.1 runnervmm7ud1 
05:08:20 dur=3.0 runnervmm7ud1 
05:08:25 dur=3.0 runnervmm7ud1 
05:08:30 dur=3.2 runnervmm7ud1 
05:08:35 dur=3.2 runnervmm7ud1 
05:08:40 dur=3.4 runnervmm7ud1 
05:08:46 dur=2.9 runnervmm7ud1 
05:08:51 dur=2.6 runnervmm7ud1 
05:08:56 dur=3.1 runnervmm7ud1 
05:09:01 dur=2.6 runnervmm7ud1 
05:09:05 dur=3.2 runnervmm7ud1 
05:09:11 dur=3.0 runnervmm7ud1 
05:09:16 dur=3.1 runnervmm7ud1 
05:09:21 dur=3.1 runnervmm7ud1 
05:09:26 dur=3.0 runnervmm7ud1 
05:09:31 dur=3.0 runnervmm7ud1 
05:09:36 dur=3.1 runnervmm7ud1 
05:09:41 dur=3.1 runnervmm7ud1 
05:09:47 dur=3.0 runnervmm7ud1 
05:09:52 dur=3.2 runnervmm7ud1 
05:09:57 dur=2.9 runnervmm7ud1 
05:10:02 dur=3.1 runnervmm7ud1 
05:10:07 dur=2.6 runnervmm7ud1 
05:10:12 dur=3.2 runnervmm7ud1 
05:10:17 dur=2.8 runnervmm7ud1 
05:10:22 dur=3.0 runnervmm7ud1 
05:10:27 dur=3.0 runnervmm7ud1 
05:10:32 dur=2.9 runnervmm7ud1 
05:10:37 dur=2.9 runnervmm7ud1 
05:10:42 dur=2.9 runnervmm7ud1 
05:10:47 dur=3.1 runnervmm7ud1 
05:10:52 dur=2.9 runnervmm7ud1 
05:10:57 dur=3.0 runnervmm7ud1 
05:11:02 dur=3.0 runnervmm7ud1 
05:11:07 dur=3.1 runnervmm7ud1 
05:11:12 dur=3.9 runnervmm7ud1 
05:11:18 dur=3.2 runnervmm7ud1 
05:11:24 dur=3.2 runnervmm7ud1 
05:11:29 dur=3.1 runnervmm7ud1 
05:11:34 dur=2.9 runnervmm7ud1 
05:11:39 dur=3.1 runnervmm7ud1 
05:11:44 dur=2.9 runnervmm7ud1 
05:11:49 dur=3.0 runnervmm7ud1 
05:11:54 dur=3.1 runnervmm7ud1 
05:12:00 dur=2.9 runnervmm7ud1 
05:12:05 dur=2.9 runnervmm7ud1 
05:12:10 dur=3.1 runnervmm7ud1 
05:12:15 dur=2.9 runnervmm7ud1 
05:12:20 dur=2.9 runnervmm7ud1 
05:12:25 dur=3.2 runnervmm7ud1 
05:12:30 dur=3.0 runnervmm7ud1 
05:12:35 dur=2.9 runnervmm7ud1 
05:12:40 dur=2.9 runnervmm7ud1 
05:12:45 dur=2.9 runnervmm7ud1 
05:12:50 dur=2.9 runnervmm7ud1 
05:12:55 dur=3.0 runnervmm7ud1 
05:13:00 dur=3.0 runnervmm7ud1 
05:13:05 dur=3.1 runnervmm7ud1 
05:13:10 dur=2.9 runnervmm7ud1 
05:13:15 dur=2.6 runnervmm7ud1 
05:13:20 dur=2.9 runnervmm7ud1 
05:13:25 dur=2.9 runnervmm7ud1 
05:13:30 dur=2.9 runnervmm7ud1 
05:13:35 dur=2.6 runnervmm7ud1 
05:13:40 dur=3.0 runnervmm7ud1 
05:13:45 dur=2.7 runnervmm7ud1 
05:13:50 dur=3.0 runnervmm7ud1 
05:13:55 dur=2.9 runnervmm7ud1 
05:14:00 dur=2.9 runnervmm7ud1 
05:14:05 dur=2.6 runnervmm7ud1 
05:14:09 dur=2.9 runnervmm7ud1 
05:14:14 dur=3.0 runnervmm7ud1 
05:14:19 dur=3.2 runnervmm7ud1 
05:14:25 dur=2.9 runnervmm7ud1 
05:14:30 dur=2.9 runnervmm7ud1 
05:14:35 dur=2.9 runnervmm7ud1 
05:14:40 dur=3.0 runnervmm7ud1 
05:14:45 dur=2.9 runnervmm7ud1 
05:14:50 dur=2.9 runnervmm7ud1 
05:14:55 dur=3.1 runnervmm7ud1 
05:15:00 dur=2.9 pb: The target machine was reached but did not open the connection. Its Paperboat service may still be starting, or it has not accepted this ma
05:15:05 dur=31.1 pb: The local Paperboat service could not complete the request (step: exec_start_uncertain). Check `pb status` for the local service and the t
05:15:38 dur=14.2 runnervmm7ud1 
```
Host log (winpool-8): worker_fenced/maintenance_busy 05:06:06-05:06:10, worker_unavailable 05:06:11-05:06:47, daemon ready 05:06:47-49. The CLI got 503 AdmissionRefusal every ~340 ms until the 10 s watchdog.

### 1b. Install C over B: old CLI vs first fix (40 s window)
install 05:14:42 -> 05:15:56, rc 0; SCM gap 05:15:08 -> 05:15:49
```
OLD
05:14:30 dur=2.5 runnervmm7ud1 
05:14:35 dur=3.1 runnervmm7ud1 
05:14:40 dur=3.0 runnervmm7ud1 
05:14:45 dur=2.9 runnervmm7ud1 
05:14:50 dur=3.1 runnervmm7ud1 
05:14:55 dur=3.2 runnervmm7ud1 
05:15:00 dur=31.4 pb: The local Paperboat service could not complete the request (step: exec_start_uncertain). Check `pb status` for the local service and the t
05:15:34 dur=18.5 runnervmm7ud1 
05:15:55 dur=2.9 runnervmm7ud1 
FIXED-40s
05:14:30 dur=2.7 runnervmm7ud1 
05:14:35 dur=3.0 runnervmm7ud1 
05:14:40 dur=3.2 runnervmm7ud1 
05:14:45 dur=3.0 runnervmm7ud1 
05:14:50 dur=2.6 runnervmm7ud1 
05:14:55 dur=2.9 runnervmm7ud1 
05:15:00 dur=50.4 pb: Paperboat is unreachable. Check your network connection and retry; if the service is recovering, retry in a moment. Support reference: sup
05:15:53 dur=3.0 runnervmm7ud1 
05:15:58 dur=3.0 runnervmm7ud1 
```
40 s missed the host's return by ~2 s; the failure text was the generic 'unreachable'. Both are fixed in the final commit (90 s; still reports start-uncertain).

### 1c. Install E over C: old CLI vs final fix
install 05:20:10 -> 05:21:15, rc 0
```
OLD
05:19:58 dur=3.5 runnervmm7ud1 
05:20:03 dur=2.7 runnervmm7ud1 
05:20:08 dur=3.2 runnervmm7ud1 
05:20:13 dur=3.1 runnervmm7ud1 
05:20:19 dur=3.1 runnervmm7ud1 
05:20:24 dur=31.1 pb: The local Paperboat service could not complete the request (step: exec_start_uncertain). Check `pb status` for the local service and the t
05:20:57 dur=13.9 runnervmm7ud1 
05:21:13 dur=3.7 runnervmm7ud1 
05:21:19 dur=3.5 runnervmm7ud1 
05:21:24 dur=3.0 runnervmm7ud1 
05:21:29 dur=3.1 runnervmm7ud1 
05:21:34 dur=3.0 runnervmm7ud1 
05:21:40 dur=2.6 runnervmm7ud1 
05:21:44 dur=2.6 runnervmm7ud1 
05:21:49 dur=2.9 runnervmm7ud1 
05:21:54 dur=3.0 runnervmm7ud1 
05:21:59 dur=3.0 runnervmm7ud1 
05:22:04 dur=2.7 runnervmm7ud1 
05:22:09 dur=3.0 runnervmm7ud1 
05:22:14 dur=2.9 runnervmm7ud1 
05:22:19 dur=3.1 runnervmm7ud1 
05:22:24 dur=3.1 runnervmm7ud1 
05:22:29 dur=2.9 runnervmm7ud1 
05:22:34 dur=2.9 runnervmm7ud1 
05:22:39 dur=3.2 runnervmm7ud1 
05:22:44 dur=2.9 runnervmm7ud1 
FIXED
05:19:58 dur=3.0 runnervmm7ud1 
05:20:03 dur=3.0 runnervmm7ud1 
05:20:08 dur=3.0 runnervmm7ud1 
05:20:13 dur=3.1 runnervmm7ud1 
05:20:18 dur=2.9 runnervmm7ud1 
05:20:23 dur=47.9 runnervmm7ud1 
05:21:13 dur=4.2 runnervmm7ud1 
05:21:19 dur=3.5 runnervmm7ud1 
05:21:25 dur=2.5 runnervmm7ud1 
05:21:29 dur=3.5 runnervmm7ud1 
05:21:35 dur=3.0 runnervmm7ud1 
05:21:40 dur=2.4 runnervmm7ud1 
05:21:44 dur=2.9 runnervmm7ud1 
05:21:49 dur=3.1 runnervmm7ud1 
05:21:55 dur=2.4 runnervmm7ud1 
05:21:59 dur=3.0 runnervmm7ud1 
05:22:04 dur=2.7 runnervmm7ud1 
05:22:09 dur=3.1 runnervmm7ud1 
05:22:14 dur=2.9 runnervmm7ud1 
05:22:19 dur=2.9 runnervmm7ud1 
05:22:24 dur=3.1 runnervmm7ud1 
05:22:29 dur=3.3 runnervmm7ud1 
05:22:35 dur=2.7 runnervmm7ud1 
05:22:39 dur=3.2 runnervmm7ud1 
05:22:45 dur=2.9 runnervmm7ud1 
```
Result: old CLI exec_start_uncertain after 31.1 s; fixed CLI, same window, succeeded after 47.9 s.

## 2. winpool-8 (Windows, fixed pb installed) -> Linux container on pbvm2 (machine_4298f3bd), pb install E over D
Each exec runs `sh -c 'echo r >> ~/w19-runs-<cli>; hostname'` so host-side runs can be counted. install 05:29:18 -> 05:29:31, rc 0
```
OLD
05:29:07 dur=3.8 071bb187cec4
05:29:13 dur=3.5 071bb187cec4
05:29:19 dur=3.5 071bb187cec4
05:29:24 dur=18.6 pb-a.exe : pb: Remote execution did not finish. Check the machine connection and retry; the remote outcome may be unknown.
05:29:45 dur=3 071bb187cec4
05:29:50 dur=3.2 071bb187cec4
05:29:55 dur=3.5 071bb187cec4
05:30:01 dur=3.3 071bb187cec4
05:30:06 dur=3 071bb187cec4
05:30:11 dur=3.1 071bb187cec4
05:30:16 dur=3.8 071bb187cec4
05:30:22 dur=3 071bb187cec4
05:30:27 dur=3.3 071bb187cec4
05:30:32 dur=3.3 071bb187cec4
05:30:37 dur=3.3 071bb187cec4
05:30:42 dur=3.5 071bb187cec4
05:30:48 dur=3.2 071bb187cec4
05:30:53 dur=3 071bb187cec4
05:30:58 dur=3.3 071bb187cec4
05:31:04 dur=2.8 071bb187cec4
FIXED
05:29:07 dur=3.5 071bb187cec4
05:29:13 dur=3.5 071bb187cec4
05:29:18 dur=3.5 071bb187cec4
05:29:24 dur=3.2 071bb187cec4
05:29:29 dur=13.9 071bb187cec4
05:29:45 dur=3.2 071bb187cec4
05:29:50 dur=3.2 071bb187cec4
05:29:55 dur=3.8 071bb187cec4
05:30:01 dur=2.7 071bb187cec4
05:30:06 dur=3.3 071bb187cec4
05:30:11 dur=2.8 071bb187cec4
05:30:16 dur=4 071bb187cec4
05:30:22 dur=3 071bb187cec4
05:30:27 dur=3.4 071bb187cec4
05:30:33 dur=2.8 071bb187cec4
05:30:37 dur=3.3 071bb187cec4
05:30:43 dur=3 071bb187cec4
05:30:48 dur=3.3 071bb187cec4
05:30:53 dur=3 071bb187cec4
05:30:58 dur=3.1 071bb187cec4
05:31:03 dur=3 071bb187cec4
RUNS
21 /home/pbt/w19-runs-fixed
20 /home/pbt/w19-runs-old
41 total
```
Result: fixed CLI 21/21 succeeded with 21 host runs (each exec ran exactly once). Old CLI 20/21 (one running exec cut: 'did not finish') with 20 runs. A Linux in-place install is short (13 s), so the start path was not hit there.

## Fresh pair control
hp -> winpool-8 right after pairing build A: 3/3 succeeded (05:04:56-05:05:03).

## hop-89 (winpool-6, official .9) read-only
hp -> machine_f7e6 failed 04:39:53-04:40:24 (meow and direct ping unanswered; slot6 logged no inbound meow from hp). The same exec succeeded at 04:57:28 and 05:25:24/27. The root cause of that ≥30 s reachability gap is still under investigation (Spark: spark/w19-slot6-meow-gap.md).
