## pb preview list

List temporary previews

### Synopsis

List previews: ID, access, target and state. Only active previews are shown by default;
--all adds previews that ended in the last 30 days, and --state picks one state. Filter with
--q and --owner; page with --cursor and --limit (1-200, default 100). Use a preview's ID with
status, inspect, replay, stop or delete.

JSON output is supported with --json.

```
pb preview list [flags]
```

### Examples

```
  pb preview list
  pb preview list --all
  pb preview list --state active --json
```

### Options

```
      --all             include ended previews (kept 30 days after they end)
      --cursor string   continue a previous page
  -h, --help            help for list
      --json            print canonical preview resources as JSON
      --limit int       maximum results (1-200) (default 100)
      --owner string    filter owner: mine, shared, or an authorized account ID
      --q string        filter by resource name or ID
      --state string    filter by resource state
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb preview](pb_preview.md)	 - Expose a local target through a temporary preview

