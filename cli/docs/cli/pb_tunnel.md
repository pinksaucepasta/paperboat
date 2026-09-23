## pb tunnel

Manage durable tunnels or start an ephemeral tunnel

### Synopsis

Manage durable tunnels or start an ephemeral tunnel

JSON output is supported with --json.

```
pb tunnel [--ephemeral] [<port|url|path>] [flags]
```

### Options

```
      --background           transfer ownership to paperboatd and return after readiness
      --domain stringArray   attach a verified custom domain (repeatable)
      --duration duration    maximum preview lifetime (deprecated: use --ttl)
      --ephemeral            use the temporary preview lifecycle
  -h, --help                 help for tunnel
      --json                 print the canonical preview resource as JSON
      --private              limit the preview to this account
      --team                 require an explicit team grant for browser access
      --ttl duration         bounded preview lifetime (maximum 24h)
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb](pb.md)	 - Open Paperboat or connect to an environment terminal
* [pb tunnel connector](pb_tunnel_connector.md)	 - Manage tunnel connectors
* [pb tunnel create](pb_tunnel_create.md)	 - Create a durable tunnel
* [pb tunnel credentials](pb_tunnel_credentials.md)	 - Manage tunnel credentials
* [pb tunnel delete](pb_tunnel_delete.md)	 - Delete endpoints and routes and revoke connectors while preserving user DNS records
* [pb tunnel doctor](pb_tunnel_doctor.md)	 - Diagnose tunnel health
* [pb tunnel domain](pb_tunnel_domain.md)	 - Manage tunnel domains
* [pb tunnel inspect](pb_tunnel_inspect.md)	 - Show daemon-local HTTP captures
* [pb tunnel list](pb_tunnel_list.md)	 - List durable tunnels
* [pb tunnel logs](pb_tunnel_logs.md)	 - Show tunnel logs
* [pb tunnel pause](pb_tunnel_pause.md)	 - Pause new traffic while preserving tunnel identity and configuration
* [pb tunnel policy](pb_tunnel_policy.md)	 - Manage permission to activate a private port on demand
* [pb tunnel replay](pb_tunnel_replay.md)	 - Deliberately replay one retained HTTP request to the same origin
* [pb tunnel resume](pb_tunnel_resume.md)	 - Resume new traffic for a preserved tunnel
* [pb tunnel route](pb_tunnel_route.md)	 - Manage tunnel routes
* [pb tunnel show](pb_tunnel_show.md)	 - Show a durable tunnel
* [pb tunnel status](pb_tunnel_status.md)	 - Show tunnel health
* [pb tunnel stop](pb_tunnel_stop.md)	 - Stop an ephemeral tunnel

