## pb ssh doctor

Check SSH integration for a machine

### Synopsis

Check SSH integration for a machine

Check the selected machine's managed SSH configuration and local integration. It reports actionable findings without approving a changed host identity. On Windows it also says when Git would use its own ssh, which cannot use the Paperboat agent, and the one command that points Git at the built-in OpenSSH: git config --global core.sshCommand C:/Windows/System32/OpenSSH/ssh.exe.

JSON output is supported with --json.

```
pb ssh doctor <machine> [flags]
```

### Examples

```
  pb ssh doctor homelab
  pb ssh doctor homelab --json
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

