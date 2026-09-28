## pb config conflict show

Show a current path conflict

### Synopsis

Show a current path conflict

Display the competing versions and status for one environment path. This is read-only and is the review step before conflict resolve.

Conflicts identify exact paths whose local and repository versions cannot be merged automatically. Review the affected path before choosing a side; resolution records a deliberate decision for the next synchronization step.

JSON output is supported with --json.

```
pb config conflict show <environment> <path> [flags]
```

### Options

```
  -h, --help   help for show
      --json   print JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb config conflict](pb_config_conflict.md)	 - Inspect and resolve configuration conflicts

