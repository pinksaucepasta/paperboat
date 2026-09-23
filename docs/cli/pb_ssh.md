## pb ssh

Connect to a machine with OpenSSH

### Synopsis

Connect to a machine with OpenSSH

This command does not support --json output; it rejects that mode before execution. Use --help --json for structured command help.

```
pb ssh [user@]<machine> [-- <OpenSSH arguments...>] [flags]
```

### Options

```
  -h, --help               help for ssh
      --transport string   peer transport: a, d, q, w, or r
      --user string        remote operating-system user
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --json               print machine-readable JSON
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb](pb.md)	 - Open Paperboat or connect to an environment terminal
* [pb ssh doctor](pb_ssh_doctor.md)	 - Check SSH integration for a machine
* [pb ssh trust-host](pb_ssh_trust-host.md)	 - Approve a changed SSH host identity

