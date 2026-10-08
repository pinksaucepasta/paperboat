## pb config conflict list

List current path conflicts

### Synopsis

List current path conflicts

List unresolved paths for one environment or across assigned environments. Each entry identifies the path that needs review before synchronization can continue.

Conflicts identify exact paths whose local and repository versions cannot be merged automatically. Review the affected path before choosing a side; resolution records a deliberate decision for the next synchronization step.

JSON output is supported with --json.

```
pb config conflict list [environment] [flags]
```

### Options

```
  -h, --help   help for list
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

* [pb config conflict](pb_config_conflict.md)	 - Inspect and resolve configuration conflicts

