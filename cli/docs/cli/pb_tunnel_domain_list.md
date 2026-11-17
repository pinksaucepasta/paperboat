## pb tunnel domain list

List tunnel domains

### Synopsis

List the hostnames a durable tunnel is served at, with the certificate state of each. Add
hostnames with pb tunnel domain add and withdraw them with pb tunnel domain remove.

JSON output is supported with --json.

```
pb tunnel domain list <tunnel> [flags]
```

### Examples

```
  pb tunnel domain list web
  pb tunnel domain list web --json
```

### Options

```
      --cursor string   continue a previous page
  -h, --help            help for list
      --json            print canonical JSON
      --limit int       maximum results (1-200) (default 100)
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

* [pb tunnel domain](pb_tunnel_domain.md)	 - Serve a tunnel at hostnames under your workspace's domains

