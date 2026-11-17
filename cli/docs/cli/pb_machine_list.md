## pb machine list

List enrolled machines

### Synopsis

List enrolled machines, one per line: name, ownership (personal or team:<team>), state, your
permissions on it, its tags and its ID. Filter with --tag (repeat it to require several tags),
--owner, --q and --state. The list stops at --limit unless you pass --all, and says how many
more exist.

With --json the output is {"version", "machines", "total"}.

JSON output is supported with --json.

```
pb machine list [flags]
```

### Examples

```
  pb machine list
  pb machine list --tag prod --tag ci
  pb machine list --state offline --json
```

### Options

```
      --all               show every item, up to 10000
  -h, --help              help for list
      --json              print JSON
      --limit int         show at most this many items (1-200) (default 50)
      --owner string      filter owner: mine, shared, or an authorized account ID
      --q string          filter by resource name or ID
      --state string      filter by resource state
      --tag stringArray   only machines with this tag (repeat for several)
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb machine](pb_machine.md)	 - Manage machines

