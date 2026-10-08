## pb tunnel connector list

List tunnel connectors

### Synopsis

List tunnel connectors

List connectors attached to one durable tunnel with cursor and limit pagination. Use the returned connector identity for drain or revoke.

Connectors attach an enrolled host to a durable tunnel. Drain stops new work while allowing existing work to wind down; revoke removes connector authority. Inspect connector state before replacing or removing one.

JSON output is supported with --json.

```
pb tunnel connector list <tunnel> [flags]
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

* [pb tunnel connector](pb_tunnel_connector.md)	 - Manage tunnel connectors

