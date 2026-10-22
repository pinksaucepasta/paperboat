## pb api-token list

List API tokens

### Synopsis

List the API tokens of the active workspace: your own, or every member's when
you are an owner or admin. The table shows each token's name, id, creator,
expiry, last use, state and whether a scope narrows it. Secrets are never
shown again after creation.

JSON output is supported with --json.

```
pb api-token list [flags]
```

### Examples

```
  pb api-token list
  pb api-token list --json
```

### Options

```
  -h, --help   help for list
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

* [pb api-token](pb_api-token.md)	 - Create, list and revoke workspace API tokens

