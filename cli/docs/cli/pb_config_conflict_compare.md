## pb config conflict compare

Read current machine and repository conflict contents through an encrypted connection

### Synopsis

Read current machine and repository conflict contents through an encrypted connection

Read both current versions of one exact conflict from the target machine through its encrypted peer connection. Content streams in bounded chunks and complete hashes are verified before display. Changed conflict or repository revisions require refreshed status. JSON output returns each verified side as base64 content; this command applies no resolution.

Conflicts identify exact paths whose local and repository versions cannot be merged automatically. Review the affected path before choosing a side; resolution records a deliberate decision for the next synchronization step.

JSON output is supported with --json.

```
pb config conflict compare <environment> <path> [flags]
```

### Options

```
  -h, --help   help for compare
      --json   print comparison content as base64 JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb config conflict](pb_config_conflict.md)	 - Inspect and resolve configuration conflicts

