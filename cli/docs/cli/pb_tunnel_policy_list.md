## pb tunnel policy list

List on-demand port policies (their IDs and generations)

### Synopsis

List your on-demand port policies with their IDs, targets, generations and expiry. Expired
ones are included. Use it to find the ID of a policy you no longer remember.

JSON output is supported with --json.

```
pb tunnel policy list [flags]
```

### Examples

```
  pb tunnel policy list
  pb tunnel policy list --json
```

### Options

```
  -h, --help   help for list
      --json   print canonical JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --ephemeral          use the temporary preview lifecycle
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb tunnel policy](pb_tunnel_policy.md)	 - Manage permission to activate a private port on demand

