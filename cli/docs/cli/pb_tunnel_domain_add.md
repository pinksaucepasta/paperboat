## pb tunnel domain add

Add a tunnel domain

### Synopsis

Add a tunnel domain

JSON output is supported with --json.

```
pb tunnel domain add <tunnel> <hostname> [flags]
```

### Options

```
  -h, --help               help for add
      --json               print canonical JSON
      --provider string    DNS provider (default "generic")
      --route string       route
      --timeout duration   maximum time to wait for operation completion (default 2m0s)
      --wait               wait for the operation to reach a terminal state
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --ephemeral          use the temporary preview lifecycle
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb tunnel domain](pb_tunnel_domain.md)	 - Manage tunnel domains

