## pb domain list

List the workspace's domains

### Synopsis

List the workspace's domains with their DNS verification state, certificate state and how many
tunnels and previews use each. With --json the output is {"items": [...]}.

JSON output is supported with --json.

```
pb domain list [flags]
```

### Examples

```
  pb domain list
  pb domain list --json
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

* [pb domain](pb_domain.md)	 - Add your own domains to the workspace

