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
  -h, --help           help for environments
      --json           print JSON
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

