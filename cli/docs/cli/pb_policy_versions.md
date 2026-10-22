## pb policy versions

List saved policy versions

### Synopsis

List every saved version of the workspace policy, newest first, with who
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
  -h, --help   help for versions
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

* [pb policy](pb_policy.md)	 - Read, test and change the workspace access policy

