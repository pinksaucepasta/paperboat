## pb tunnel domain remove

Stop serving a tunnel at a hostname

### Synopsis

Stop serving a tunnel at a hostname. The workspace domain and its DNS records stay.

Confirmation is two steps. Run the command without --confirm: it changes nothing, shows what
would happen and prints a six-character code (exit status 2). Run the same command again with
--confirm CODE within 5 minutes. The code works once and only for that exact command and target.

JSON output is supported with --json.

```
pb tunnel domain remove <tunnel> <hostname> [flags]
```

### Examples

```
  pb tunnel domain remove web app.example.com
  pb tunnel domain remove web app.example.com --confirm A1B2C3
```

### Options

```
      --confirm string     six-character confirmation code from the preview
  -h, --help               help for remove
      --json               print canonical JSON
      --timeout duration   maximum time to wait for operation completion (default 2m0s)
      --wait               wait for the operation to reach a terminal state
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

