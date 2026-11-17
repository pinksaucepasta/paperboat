## pb tunnel

Manage durable tunnels or start an ephemeral tunnel

### Synopsis

Manage durable tunnels, or start a temporary preview. A durable tunnel keeps its address,
routes, domains and access rules across sessions and restarts; manage it with create, list,
show, route, domain, policy, pause, resume and delete. pb tunnel with no subcommand prints help.

With --ephemeral and a target (a port, URL or path) the command behaves exactly like pb preview:
a temporary preview with its own lifecycle. pb tunnel stop stops only previews; a durable tunnel
is paused or deleted.

JSON output is supported with --json.

```
pb tunnel [--ephemeral] [<port|url|path>] [flags]
```

### Examples

```
  pb tunnel create web --port 8080
  pb tunnel list
  pb tunnel --ephemeral 3000 --private
  pb tunnel stop preview_abc123
```

### Options

```
      --background           hand the preview to this computer's background service and return once ready (default lifetime 30m)
      --domain stringArray   attach a verified custom domain (repeatable)
      --ephemeral            use the temporary preview lifecycle
  -h, --help                 help for tunnel
      --host-header string   send this Host to the origin instead of the public host (http, https, h2c targets; for dev servers with a host allow-list)
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
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb](pb.md)	 - Open Paperboat or connect to an environment terminal
* [pb tunnel create](pb_tunnel_create.md)	 - Create a durable tunnel
* [pb tunnel delete](pb_tunnel_delete.md)	 - Delete endpoints and routes while preserving user DNS records
* [pb tunnel doctor](pb_tunnel_doctor.md)	 - Diagnose tunnel health
* [pb tunnel domain](pb_tunnel_domain.md)	 - Serve a tunnel at hostnames under your workspace's domains
* [pb tunnel extend](pb_tunnel_extend.md)	 - Move a durable tunnel's expiry to now plus a duration
* [pb tunnel inspect](pb_tunnel_inspect.md)	 - Show daemon-local HTTP captures
* [pb tunnel ip-rules](pb_tunnel_ip-rules.md)	 - Show or set which client addresses may open this tunnel
* [pb tunnel list](pb_tunnel_list.md)	 - List durable tunnels
* [pb tunnel logs](pb_tunnel_logs.md)	 - Show tunnel logs
* [pb tunnel pause](pb_tunnel_pause.md)	 - Pause new traffic while preserving tunnel identity and configuration
* [pb tunnel policy](pb_tunnel_policy.md)	 - Manage permission to activate a private port on demand
* [pb tunnel rename](pb_tunnel_rename.md)	 - Rename a durable tunnel
* [pb tunnel replay](pb_tunnel_replay.md)	 - Deliberately replay one retained HTTP request to the same origin
* [pb tunnel resume](pb_tunnel_resume.md)	 - Resume new traffic for a preserved tunnel
* [pb tunnel route](pb_tunnel_route.md)	 - Manage tunnel routes
* [pb tunnel show](pb_tunnel_show.md)	 - Show a durable tunnel
* [pb tunnel status](pb_tunnel_status.md)	 - Show tunnel health
* [pb tunnel stop](pb_tunnel_stop.md)	 - Stop an ephemeral tunnel

