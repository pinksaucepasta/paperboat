## pb domain add

Add a domain and show its DNS records

### Synopsis

Add a domain you control to the active workspace and print the DNS records to create at your
DNS provider: a CNAME that proves you control it, and the records that point names at
Paperboat. Then run pb domain verify. Once the domain is verified, any name one level under it
works with --domain on tunnels and previews.

JSON output is supported with --json.

```
pb domain add <domain> [flags]
```

### Examples

```
  pb domain add dev.example.com
  pb domain add dev.example.com --json
```

### Options

```
  -h, --help   help for add
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

