## pb domain remove

Remove a domain no tunnel or preview uses

### Synopsis

Remove a domain from the workspace. A domain that a tunnel or preview still uses is refused,
and the message names the hostnames to remove first (pb tunnel domain remove). DNS records at
your provider are not touched; delete them there.

Confirmation is two steps. Run the command without --confirm: it changes nothing, shows what
would happen and prints a six-character code (exit status 2). Run the same command again with
--confirm CODE within 5 minutes. The code works once and only for that exact command and target.

JSON output is supported with --json.

```
pb domain remove <domain> [flags]
```

### Examples

```
  pb domain remove dev.example.com
  pb domain remove dev.example.com --confirm A1B2C3
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

