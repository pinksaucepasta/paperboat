## pb domain

Add your own domains to the workspace

### Synopsis

Add a domain you control (example.com or dev.example.com) once. After its DNS records
are in place and it shows verified, any name one level under it (app.example.com) can be used by
tunnels and previews with --domain, with HTTPS from one shared certificate.

Add domains you control to the active workspace once, then use any name one level under a verified domain for tunnels and previews (--domain). Team domains are added and removed by owners and admins.

With --json, this command group lists its available commands.

```
pb domain [flags]
```

### Options

```
  -h, --help   help for domain
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --json               print machine-readable JSON
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb](pb.md)	 - Open Paperboat or connect to an environment terminal
* [pb domain add](pb_domain_add.md)	 - Add a domain and show its DNS records
* [pb domain list](pb_domain_list.md)	 - List the workspace's domains
* [pb domain remove](pb_domain_remove.md)	 - Remove a domain no tunnel or preview uses
* [pb domain show](pb_domain_show.md)	 - Show a domain's DNS records, status and uses
* [pb domain verify](pb_domain_verify.md)	 - Check a domain's DNS now and wait until it is verified

