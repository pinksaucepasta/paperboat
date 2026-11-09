## pb domain list

List the workspace's domains

### Synopsis

List the workspace's domains

List the active workspace's domains with their DNS verification state, certificate state and how many tunnel or preview hostnames use each one. Use --json for the full records and uses.

JSON output is supported with --json.

```
pb domain list [flags]
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

