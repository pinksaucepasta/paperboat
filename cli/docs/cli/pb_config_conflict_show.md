## pb config conflict show

Show a current path conflict

### Synopsis

Show a current path conflict

Display the path, reason and current status for one environment conflict. Use compare to read the actual machine and repository content before choosing which version to keep with resolve.

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
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb config conflict](pb_config_conflict.md)	 - Inspect and resolve configuration conflicts

