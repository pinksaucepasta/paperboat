## pb policy versions

List saved policy versions

### Synopsis

List the saved versions of the workspace policy, newest first (the latest 50;
--limit or --all for more), with who
saved it, when, how (saved, restored, rewritten or migrated), how many access
grants it added and removed, and its message. Print one with pb policy get
--version N and go back to one with pb policy restore N.

JSON output is supported with --json.

```
pb policy versions [flags]
```

### Examples

```
  pb policy versions
  pb policy versions --json
```

### Options

```
      --all         show every item, up to 10000
  -h, --help        help for versions
      --json        print JSON
      --limit int   show at most this many items (1-200) (default 50)
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb policy](pb_policy.md)	 - Read, test and change the workspace access policy

