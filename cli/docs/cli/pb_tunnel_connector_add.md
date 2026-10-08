## pb tunnel connector add

Add a connector on this host

### Synopsis

Add a connector on this host

Add a connector for the selected durable tunnel on this host. Inspect tunnel and connector status before advertising a route through the new attachment.

Connectors attach an enrolled host to a durable tunnel. Drain stops new work while allowing existing work to wind down; revoke removes connector authority. Inspect connector state before replacing or removing one.

JSON output is supported with --json.

```
pb tunnel connector add <tunnel> [flags]
```

### Options

```
  -h, --help   help for add
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

* [pb tunnel connector](pb_tunnel_connector.md)	 - Manage tunnel connectors

