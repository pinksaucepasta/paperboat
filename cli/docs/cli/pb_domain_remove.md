## pb domain remove

Remove a domain no tunnel or preview uses

### Synopsis

Remove a domain no tunnel or preview uses

Remove a domain after confirmation. Refused while a tunnel or preview uses a name under it; your DNS records stay at your provider.

JSON output is supported with --json.

```
pb domain remove <domain> [flags]
```

### Options

```
      --confirm string   six-character confirmation code from the preview
  -h, --help             help for remove
      --json             print JSON
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

