## pb team activity

View owner/admin activity from the last 90 days

### Synopsis

View owner/admin activity from the last 90 days

JSON output is supported with --json.

```
pb team activity <team> [flags]
```

### Options

```
      --cursor string   next_cursor from the previous page
  -h, --help            help for activity
      --json            print canonical JSON including metadata and next_cursor
      --limit int       maximum events, 1–200 (default 50)
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb team](pb_team.md)	 - Manage teams and explicit resource permissions

