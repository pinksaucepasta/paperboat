## pb port front-desk

Show or set where unnamed addresses of a machine go

### Synopsis

A machine's front desk is the port where <name>.<machine>.<domain> goes when no
port has that name: a reverse proxy such as Coolify, Traefik or Caddy. The default is
port 80 over http. Machine managers set it; the scheme defaults to https for port 443.

JSON output is supported with --json.

```
pb port front-desk <machine> [<port> [http|https]] [flags]
```

### Examples

```
  pb port front-desk homelab
  pb port front-desk homelab 443 https
```

### Options

```
  -h, --help   help for front-desk
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

* [pb port](pb_port.md)	 - Name machine ports and set a machine's front desk

