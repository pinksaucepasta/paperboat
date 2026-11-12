## pb ssh

Connect to a machine with OpenSSH

### Synopsis

Connect to a machine with OpenSSH over Paperboat.

Forwarding follows the machine's sshd: port and agent forwarding work as it allows; pb never requests X11 and does not strip -X/-Y. Windows machines cannot forward an agent (a Microsoft OpenSSH limitation); port forwarding works there.

On Windows, Git for Windows' own ssh cannot use the Paperboat agent: to run git over ssh to your machines, point Git at the built-in OpenSSH once with `git config --global core.sshCommand C:/Windows/System32/OpenSSH/ssh.exe`. pb ssh doctor says so when it applies.

This command does not support --json output; it rejects that mode before execution. Use --help --json for structured command help.

```
pb ssh [user@]<machine> [-- <OpenSSH arguments...>] [flags]
```

### Options

```
  -h, --help          help for ssh
      --user string   remote operating-system user
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --json               print machine-readable JSON
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb](pb.md)	 - Open Paperboat or connect to an environment terminal
* [pb ssh doctor](pb_ssh_doctor.md)	 - Check SSH integration for a machine
* [pb ssh trust-host](pb_ssh_trust-host.md)	 - Approve a changed SSH host identity

