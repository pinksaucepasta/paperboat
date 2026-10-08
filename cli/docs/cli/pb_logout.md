## pb logout

Revoke and remove the active client session

### Synopsis

Revoke and remove the active client session

Revoke and remove the current CLI client session. This top-level command has the same account effect as pb auth logout and reports any pending server revocation.

JSON output is supported with --json.

```
pb logout [flags]
```

### Options

```
  -h, --help   help for logout
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

