## pb environments

List enrolled machines available to this account

### Synopsis

List enrolled machines available to this account

List enrolled machines available to this account as terminal environments. Use the displayed name or ID with pb connect; visibility does not imply a machine is ready.

JSON output is supported with --json.

```
pb environments [flags]
```

### Options

```
      --all            show every item, up to 10000
  -h, --help           help for environments
      --json           print JSON
      --limit int      show at most this many items (1-200) (default 50)
      --owner string   filter owner: mine, shared, or an authorized account ID
      --q string       filter by resource name or ID
      --state string   filter by resource state
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

