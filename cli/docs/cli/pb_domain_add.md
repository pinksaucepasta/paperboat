## pb domain add

Add a domain and show its DNS records

### Synopsis

Add a domain and show its DNS records

Add an apex or subdomain and print the DNS records to create: the _acme-challenge CNAME that proves control, and the wildcard record that sends names under it to Paperboat.

JSON output is supported with --json.

```
pb domain add <domain> [flags]
```

### Examples

```
  pb domain add dev.example.com
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

