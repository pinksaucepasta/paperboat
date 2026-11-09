## pb domain verify

Check a domain's DNS now and wait until it is verified

### Synopsis

Check a domain's DNS now and wait until it is verified

Ask the server to check the domain's DNS now and wait (up to --timeout) until it is verified and its certificate is issued, or show why not.

JSON output is supported with --json.

```
pb domain verify <domain> [flags]
```

### Options

```
  -h, --help               help for verify
      --json               print JSON
      --timeout duration   how long to wait for verification and the certificate (default 2m0s)
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

