## pb tunnel policy get

Show an on-demand port policy

### Synopsis

Show one on-demand port policy: its target, access mode, state, ID and generation. Use the ID
and generation to update it with pb tunnel policy allow or end it with pb tunnel policy revoke.

JSON output is supported with --json.

```
pb tunnel policy get <policy> [flags]
```

### Examples

```
  pb tunnel policy get pol_abc
  pb tunnel policy get pol_abc --json
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

