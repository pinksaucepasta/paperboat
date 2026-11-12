# X-44 part 2 by hand: the Windows host with two OS users reached from Linux, UTC 20261112 22:00 (IST 03:30 13 Nov)

Windows host (hosted runner): runneradmin (account B, machine xpq-w) and ZoeC (account C, machine xpq-wc, both online this time); Linux client users tester (B) and tester2 (C).
```
tester  (B): pb exec xpq-w  -> runnervmm7ud1\runneradmin      pb exec xpq-wc -> "No machine has that name or ID"
tester2 (C): pb exec xpq-wc -> runnervmm7ud1\zoec             pb exec xpq-w  -> "No machine has that name or ID"
```
The second Windows OS user's machine came online on this fresh runner (it stayed offline on the earlier runner, failures.md). Not run: the refusal page for a system or unknown user on the Windows gateway (the scheduled-task helper produced no output), any macOS host or client.
