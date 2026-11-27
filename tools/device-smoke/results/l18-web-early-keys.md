# L18 browser proof: keys typed into the web quick terminal before it attaches

Console fix: 6f878a08d (on origin/main). Live origin: https://app.paperboat.cloud (server 2026.11.27.2, per coordinator).

Machines (test only):
- Linux: test-1127-hp = `machine_18831cdf-67e8-4b0f-830a-f3910df44022`
- Windows: test-1127-l18w = `machine_bed7a60c-860f-474d-93d7-491886a06699` (winpool-1, dev-cc7d5eaf2)

Browsers: Playwright 1.65.1 (playwright-core), Chromium 159.0.8154.6, WebKit 27.2, both run on oracle (aarch64). One browser at a time, started only at 1-min load <= 12.

Session: the `SMOKE_OPERATOR_STATE` file from the smoke targets (path not recorded). It was never copied off oracle. The session file was rotated at 09:52:11; the live batch below ran after that, on the rotated file.

## Before the fix (old live console, before the deploy)

Run from linuxpool-12 with `check-quick-terminal-early-input.mjs` (TRIALS=2, ticket delayed 4000 ms; first version of
the script, free test account, Linux container target): WebKit **0 of 4** lines ran. All four were typed in phase
`connecting` (2 first connects, 2 reconnect attempts); the screen showed only the bare prompt. The Chromium and
Windows legs of that run did not finish (run time limit; Windows runner lease over), so there is no before result for
them. An earlier run on the old console typed before the attach and lost every line on Linux and Windows
(WebKit and Chromium, 0 of 5 each), and lost none of the lines typed after the prompt had appeared.

## Which bundle each run used

- **Live, unmodified check:** the check script `console/scripts/check-quick-terminal-early-input.mjs` and `check-quick-terminal-scroll.mjs`, run against https://app.paperboat.cloud. Every run in the "Live" tables below used this.
- **Fixed-bundle check (interception):** not completed (see "Not done").
- **Live terminal JS:** `0pgdwy4k2nl73.js` on the live origin contains "Keys typed now are sent once the terminal is connected". Checked at about 09:46, before the coordinator's SMOKE-READY notice. The local copy of the console build has the same chunk name.

## Run A: BEFORE (unmodified, live), TRIALS=2, TICKET_DELAY_MS=4000

Started at 09:51, before the session rotation finished. Expected FAIL, but the live terminal already contained the fix string, so the expectation was wrong.

| browser | machine | result | lines intact | notes |
|---|---|---|---|---|
| chromium | Linux | FAIL (trial 2) | 2 of 2 in trial 1; trial 2 never reached the terminal | trial 2: `waitForSelector('.terminal-host textarea')` timed out after 90 s, a page-load timeout, not a typing failure |
| chromium | Windows | PASS | 4 of 4 | |
| webkit | Linux | PASS | 4 of 4 | |
| webkit | Windows | PASS | 4 of 4 | |

All lines that were typed were typed in phase `connecting` (the ticket delay), on the first connect and on the reconnect.

## Run B: LIVE, unmodified, TRIALS=5, TICKET_DELAY_MS=4000 (default)

| browser | machine | result | lines intact |
|---|---|---|---|
| chromium | Linux | PASS | 10 of 10 |
| chromium | Windows | PASS | 10 of 10 |
| webkit | Linux | PASS | 10 of 10 |
| webkit | Windows | PASS | 10 of 10 |

## Run C: LIVE, unmodified, TRIALS=5, TICKET_DELAY_MS=0

| browser | machine | result | lines intact |
|---|---|---|---|
| chromium | Linux | PASS | 10 of 10 |
| chromium | Windows | PASS | 10 of 10 |
| webkit | Linux | PASS | 10 of 10 |
| webkit | Windows | PASS | 10 of 10 |

## Run D: LIVE, unmodified, scroll check

Linux: `seq 500`. Windows: `dir C:\Windows\System32\*.exe`. Docked right and docked bottom. Height stayed fixed and the scroll moved the scrollback, not the page (page top stayed at 0).

| browser | machine | result |
|---|---|---|
| chromium | Linux | PASS |
| chromium | Windows | PASS |
| webkit | Linux | PASS |
| webkit | Windows | PASS |

## Not done / deviations

0. Likely cause of the Run A timeout (inferred from the times, not proven): that run started at 09:51 with the old
   session, which was signed out when the session was rotated (09:52:11); later runs read the new file and passed.

1. **Premise wrong: the live console already had the fix.** The BEFORE run was expected to FAIL but passed on webkit (both machines) and chromium Windows. The one chromium Linux failure is the 90 s terminal-mount timeout in trial 2, not a typing failure; it did not recur in Run B or C (20/20 on that machine).
2. **Fixed-bundle interception (run b) not done.** Local serve of the copied console build from linuxpool-5 mounted the page shell, but two chunks it requested returned 404 and the terminal never attached (trial 1 timed out at 90 s). That tree is `d06bcdd`, which is not a descendant of 6f878a08d, so it was not used as a fixed build. Runs B and C replaced it, since live now serves the fixed bundle.
3. **Held-keys notice and 10 s-later probe not captured in live runs.** The unmodified check does not record them, and no line failed in the live runs.
4. **Reconnect about 30 s after opening (`data-terminal-close`) not measured** in the unmodified checks.
5. **Leftover server on linuxpool-5:3100 killed** (`next start` pid 47816/47817). The port is free.

## Commands (what ran)

Unmodified checks, both browsers, both machines, from the worktree:

```
STATE=$SMOKE_OPERATOR_STATE CONSOLE_ORIGIN=https://app.paperboat.cloud MACHINE=<machine id> \
  BROWSERS=chromium TRIALS=5 TICKET_DELAY_MS=4000 \
  PW_MODULE=/home/ubuntu/.config/paperboat-dev/pw/node_modules/playwright-core/index.mjs \
  node console/scripts/check-quick-terminal-early-input.mjs
```

(`BROWSERS=webkit` for the other browser; `TICKET_DELAY_MS=0` for Run C.)

Scroll check:

```
STATE=$SMOKE_OPERATOR_STATE CONSOLE_ORIGIN=https://app.paperboat.cloud MACHINE=<machine id> \
  BROWSERS=chromium CMD='seq 500' \
  PW_MODULE=... node console/scripts/check-quick-terminal-scroll.mjs
```

(Windows: `CMD='dir C:\Windows\System32\*.exe'`.)

Windows runner install:

```
BUILD_DIR=/home/ubuntu/paperboat-private/clean-smoke-1127/builds/dev-cc7d5eaf2 \
SMOKE_HARNESS=<worktree>/tools/device-smoke MINT_HOST=anvit@hp \
  pool-install.sh "$BUILD_DIR" dev-cc7d5eaf2 winpool-1=test-1127-l18w
```

Result: PASS, `machine_bed7a60c-860f-474d-93d7-491886a06699`.

## Cleanup

- Linux runner test-1127-hp: not touched by this proof beyond the checks above (no record change).
- Windows runner winpool-1 (test-1127-l18w): **released** (`runner free winpool-1`, now FREE). **Record NOT revoked:** `cleanup_test_account.py --id machine_bed7a60c-…` matched 0 machines at `--stale '0 seconds'` and at `'1 minute'` (dry-run; the record is online, alias matches the throwaway pattern, so its account is likely outside the configured cleanup accounts). The record needs revoking by an operator.
- Local next server on oracle (127.0.0.1:3900): stopped.
- Scratch copies (console build copy, node_modules symlink, diagnostic scripts): deleted; the symlink target in /home/ubuntu/paperboat/console/node_modules is intact. Run logs remain in the scratchpad `logs/` directory.
