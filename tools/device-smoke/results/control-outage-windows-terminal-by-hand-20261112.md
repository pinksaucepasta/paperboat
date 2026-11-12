F21-01 W terminal part, checked by hand during a control-plane cut (user rule 2026-11-12: do not script the cmd.exe terminal check).
Machines: win-stg-w2 (hosted Windows runner winpool-8, pb dev-abf41fa69 = current main, staging server) and the Linux container win-stg-l (dev-bdb1db804). Cut: pb.exe on the runner blocked to the API port, and the API port dropped in the container (the rest of the staging host, including the relay, reachable); 30 s into the cut.
Command (from the container): `pb connect win-stg-w2` in a pty; the prompt appears, then typed `echo 42` + Enter.
Observed transcript (trimmed): "Microsoft Windows [Version 10.0.26100.33674]" / "C:\Users\runneradmin>echo 42" / "42" / "C:\Users\runneradmin>" and `exit` closes it; pb warned "ENV not applied: ENV needs Paperboat online" (expected during the cut).
Result: the terminal opens through the relay with the control plane unreachable and runs cmd.exe commands (42 printed).
