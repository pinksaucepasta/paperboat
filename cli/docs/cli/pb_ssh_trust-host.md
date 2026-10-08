## pb ssh trust-host

Approve a changed SSH host identity

### Synopsis

Approve a changed SSH host identity

Approve one pending SSH host-key change by its exact SHA256 fingerprint. Verify the fingerprint out of band before accepting it; this changes trust for the selected machine.

Managed SSH resolves the Paperboat machine and then runs OpenSSH with the selected target. Host identity checks remain in force. Standard SSH, SCP, SFTP, and rsync behavior is preserved while Paperboat supplies machine resolution and authorized connectivity.

JSON output is supported with --json.

```
pb ssh trust-host <machine> [flags]
```

### Options

```
      --fingerprint string   exact pending SHA256 fingerprint
  -h, --help                 help for trust-host
      --json                 print JSON
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

