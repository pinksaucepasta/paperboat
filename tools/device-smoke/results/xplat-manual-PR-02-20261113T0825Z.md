# PR-02 M and W: outbound UDP blocked except 53/443, carrier falls back, `pb exec` works (2026-11-13 08:25 UTC)

Machines: macpool-3 (macOS 26.7, arm64, GitHub runner) = `macpool-relpr02-3`; winpool-9 (Windows Server 2025) = `winpool-relpr02-1`.
Both `pb version` = dev-6c489c5f5 (newest .done build, tools from main 4da4ecfd9). Both paired in one account.

## Workspace tests, compiled and run natively from main 4da4ecfd9 (`go test -count=1 -v -run ... ./internal/hostruntime/connector/`)
- Windows (go1.27.4 windows/amd64): TestHTTP3FailureFallsBackOnImmediateUDPRefusal PASS, TestDataCarrierPoolPrefersHTTP2WhileUDPIsRefused PASS
- macOS (go1.27.4 darwin/arm64): TestHTTP3FailureFallsBackOnImmediateUDPRefusal PASS, TestDataCarrierPoolPrefersHTTP2WhileUDPIsRefused PASS

## W: Windows blocked
- Block: `New-NetFirewallRule -Direction Outbound -Protocol UDP -Action Block -RemotePort 1-52,54-442,444-65535` (UDP 53 and 443 stay open), carrier services restarted (`Restart-Service PaperboatCarrier-*`); removal task armed for +6 min.
- `pb exec winpool-relpr02-1 -- hostname` from macpool-3: OK on the first try, 18 s after the block was applied (output = the Windows host name).
- `pb status` on macpool-3 during the block: winpool-relpr02-1 Runtime ready, SSH ready.
- Block removed; exec both ways OK; enrollment unchanged (same machine ids, no re-pair).

## M: Mac blocked
- Block: pf `block drop out quick proto udp from any to any port { 1:52, 54:442, 444:65535 }` (pfctl -f - / -E; 53 and 443 pass), carrier LaunchDaemon kickstarted; pf counters showed 202 UDP packets dropped.
- `pb exec macpool-relpr02-3 -- hostname` from winpool-9: first try at 21 s failed ("could not reach the target", carrier still redialling); OK at 63 s after the block (output = the Mac host name).
- `pb exec winpool-relpr02-1 -- hostname` from the blocked Mac: first try at 47 s failed, second OK at 56 s.
- `pb status` on the blocked Mac: winpool-relpr02-1 `Path: wss/india` (TCP, no UDP); recovery inside udp_blocked.py's 90 s budget.
- pf disabled; exec both ways OK; enrollment unchanged.

The HTTP/2 choice itself is not shown by any status/doctor field or log (carrier transport is not exposed); it is proven by the two workspace tests above plus the TCP-only path with UDP dropped.
