## pb domain show

Show a domain's DNS records, status and uses

### Synopsis

Show one domain: its DNS records, verification state, certificate state and expiry, the fix
for a failed check, and the tunnels and previews that use it. Name it by hostname or ID; case
and a trailing dot are ignored.

JSON output is supported with --json.

```
pb domain show <domain> [flags]
```

### Examples

```
  pb domain show dev.example.com
  pb domain show dev.example.com --json
```

### Options

```
  -h, --help   help for show
      --json   print JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb domain](pb_domain.md)	 - Add your own domains to the workspace

