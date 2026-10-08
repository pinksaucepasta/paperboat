## pb ssh doctor

Check SSH integration for a machine

### Synopsis

Check SSH integration for a machine

Check the selected machine's managed SSH configuration and local integration. It reports actionable findings without approving a changed host identity.

Managed SSH resolves the Paperboat machine and then runs OpenSSH with the selected target. Host identity checks remain in force. Standard SSH, SCP, SFTP, and rsync behavior is preserved while Paperboat supplies machine resolution and authorized connectivity.

JSON output is supported with --json.

```
pb ssh doctor <machine> [flags]
```

### Options

```
  -h, --help   help for doctor
      --json   print JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb ssh](pb_ssh.md)	 - Connect to a machine with OpenSSH

