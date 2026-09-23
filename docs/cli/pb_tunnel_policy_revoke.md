## pb tunnel policy revoke

Revoke an on-demand port policy

### Synopsis

Revoke an on-demand port policy

JSON output is supported with --json.

```
pb tunnel policy revoke <policy> [flags]
```

### Options

```
      --generation int   expected policy generation; omitted reads current state first
  -h, --help             help for revoke
      --json             print canonical JSON
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

