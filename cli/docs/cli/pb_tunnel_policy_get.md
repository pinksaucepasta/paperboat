## pb tunnel policy get

Show an on-demand port policy

### Synopsis

Show an on-demand port policy

Show one on-demand port policy, including its target and authorization state. Use its identity and generation for a deliberate revoke.

An on-demand policy authorizes an exact machine port for private or team access. A policy does not publish a public URL or grant every port on the machine. Revocation fences subsequent activation.

JSON output is supported with --json.

```
pb tunnel policy get <policy> [flags]
```

### Options

```
  -h, --help   help for get
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

* [pb tunnel policy](pb_tunnel_policy.md)	 - Manage permission to activate a private port on demand

