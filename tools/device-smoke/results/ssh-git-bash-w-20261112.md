# X-28 on Windows: the managed ssh config from Git Bash's ssh, 2026-11-12 (winpool runner, Windows Server 2025, build of main with 9bc7cdad6)
Runner paired with a prod test account; `pb` wrote ~/.ssh/config (Include) and ~/.ssh/paperboat_config. Target: a Linux machine of the account (alias target.example.test, user pbt).

| ssh program | result |
|---|---|
| built-in Microsoft OpenSSH_for_Windows_10.0p2, from PowerShell | `ssh pbt@target.example.test hostname` prints the target's hostname: PASS |
| Git for Windows MSYS OpenSSH_10.6p1, from Git Bash (`bash -lc`, SHELL=/usr/bin/bash) | ProxyCommand runs, the connection opens, KnownHostsCommand verifies the host key, then `Permission denied (publickey,password)`: FAIL at authentication |

Why Git ssh stops at authentication: the managed config names `IdentityAgent "\\.\pipe\Paperboat-SSH-Agent"`, a Windows named pipe. The same agent answers the built-in `ssh-add -l` (lists the managed key), but MSYS `ssh-add -l` and ssh fail on it every time with "Error connecting to agent: Device or resource busy" (MSYS OpenSSH cannot speak to a Windows named pipe agent). The `.pub` identity file itself loads (`ssh-keygen -l` ok).

What 9bc7cdad6 fixed (before it: "KnownHostsCommand-ORDER path is not absolute", Host key verification failed): the executable path form `//?/C:/Program Files/.../pb.exe` is accepted by both programs for ProxyCommand and KnownHostsCommand (matrix: ssh-path-forms-w-20261112.md). Note for harnesses: run Git ssh in a real Git Bash. Started from a PowerShell session whose SHELL names cmd.exe, MSYS ssh runs a ProxyCommand through an interactive cmd.exe and fails for any path form.

Open decision (product): Git for Windows ssh cannot use the managed key until the agent is reachable the way MSYS ssh reaches agents (an AF_UNIX/Cygwin socket), and one shared config cannot name both agent kinds without a Match exec. Until then the supported way to use the managed hosts from Git (git clone, git push over ssh) is `git config --global core.sshCommand C:/Windows/System32/OpenSSH/ssh.exe`.
