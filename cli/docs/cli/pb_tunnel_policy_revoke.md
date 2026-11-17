## pb tunnel policy revoke

Revoke an on-demand port policy

### Synopsis

Revoke one on-demand port policy so later activations are denied. --generation is the
generation you expect; without it the command reads the current one first. Unrelated tunnel
routes are not affected.

JSON output is supported with --json.

```
pb tunnel policy revoke <policy> [flags]
```

### Examples

```
  pb tunnel policy revoke pol_abc
  pb tunnel policy revoke pol_abc --generation 3
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
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb tunnel policy](pb_tunnel_policy.md)	 - Manage permission to activate a private port on demand

