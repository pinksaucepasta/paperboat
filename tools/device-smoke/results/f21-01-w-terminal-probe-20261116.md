# F21-01 W: new `pb connect` terminals during an 840 s control-plane cut (by hand, 2026-11-16)

Machines: W = relf2101-w-2 (winpool-8, Windows Server 2025 10.0.26100, GitHub-hosted runner), L = relf2101-l (Linux container on pbvm2),
staging server anvit; `pb version` of both: dev-06bdf03b5. Cut: W's pb.exe blocked from the API port (firewall) and L's API port
dropped (iptables); the relay stays reachable. Each line: a new `pb connect relf2101-w-2` from L on a pty, wait for the cmd.exe
prompt, type `echo pN-%SystemRoot%`, PASS when `pN-C:\Windows` is printed by the shell on W, then `exit`; and a new `pb exec` L->W.

```
cut rc 0
+16s terminal: prompt_after=31.5s result=PASS | exec L->W: True
+84s terminal: prompt_after=31.7s result=PASS | exec L->W: True
+153s terminal: prompt_after=31.8s result=PASS | exec L->W: True
+222s terminal: prompt_after=31.5s result=PASS | exec L->W: True
+290s terminal: prompt_after=31.4s result=PASS | exec L->W: True
+359s terminal: prompt_after=31.5s result=PASS | exec L->W: True
+429s terminal: prompt_after=31.4s result=PASS | exec L->W: True
+498s terminal: prompt_after=31.7s result=PASS | exec L->W: True
+566s terminal: prompt_after=55.1s result=PASS | exec L->W: True
+688s terminal: prompt_after=61.6s result=PASS | exec L->W: True
restored after 824
```

The same check is now part of control_outage_windows.py (it waits for the prompt before typing; typing at a fixed 15 s reached a
tty that was not yet raw and Enter was lost). The terminal attaches after 31-62 s in an outage (cause not traced; the constant
31.5 s suggests one 30 s control-plane timeout before the CLI opens the session from the host's rules).
