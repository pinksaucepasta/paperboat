## pb tunnel policy allow

Allow on-demand access to an exact machine port

### Synopsis

Allow on-demand access to an exact machine port

JSON output is supported with --json.

```
pb tunnel policy allow <machine> <port|url> [flags]
```

### Options

```
      --access string      access audience: private or team (default "private")
      --expires duration   policy lifetime (default 8h0m0s)
      --generation int     expected existing policy generation
  -h, --help               help for allow
      --id string          existing policy ID to update
      --json               print canonical JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --ephemeral          use the temporary preview lifecycle
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb tunnel policy](pb_tunnel_policy.md)	 - Manage permission to activate a private port on demand

