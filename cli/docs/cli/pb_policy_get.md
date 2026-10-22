## pb policy get

Print the saved workspace policy

### Synopsis

Print the saved policy file of the active workspace exactly as it was saved,
comments included, so it can be edited and saved again with pb policy set.
Use --version to print an earlier version; pb policy versions lists them. With
--json, print the whole policy record: version, digest, author and the names
the policy may use.

JSON output is supported with --json.

```
pb policy get [flags]
```

### Examples

```
  pb policy get > policy.hujson
  pb policy get --version 3
  pb policy get --json
```

### Options

```
  -h, --help          help for get
      --json          print JSON
      --version int   print this saved version instead of the current one
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

