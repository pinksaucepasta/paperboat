## pb status

Show local Paperboat machine status

### Synopsis

Show this machine's Paperboat state, or the observed status of the machine you name. It is
a snapshot, not a wait: use pb doctor for a deeper check, pb service status for the background
service and pb wait to wait for a condition. On a machine that is waiting to be added, status
reports the signed-out state; run pb login.

JSON output is supported with --json.

```
pb status [machine] [flags]
```

### Examples

```
  pb status
  pb status studio
  pb status --json
```

### Options

```
  -h, --help   help for status
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

* [pb](pb.md)	 - Open Paperboat or connect to an environment terminal

