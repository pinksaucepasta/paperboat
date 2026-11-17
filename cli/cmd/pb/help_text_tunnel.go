package main

// waitNote is shared by the tunnel commands that change a durable tunnel.
const waitNote = `A change returns once the request is accepted, and the tunnel may still be connecting. Pass
--wait to wait (at most --timeout, default 2m, 1s to 10m) until it finishes, or check pb tunnel
status.`

var tunnelHelpTexts = map[string]helpText{
	"pb tunnel": {
		Long: `Manage durable tunnels, or start a temporary preview. A durable tunnel keeps its address,
routes, domains and access rules across sessions and restarts; manage it with create, list,
show, route, domain, policy, pause, resume and delete. pb tunnel with no subcommand prints help.

With --ephemeral and a target (a port, URL or path) the command behaves exactly like pb preview:
a temporary preview with its own lifecycle. pb tunnel stop stops only previews; a durable tunnel
is paused or deleted.`,
		Example: `
  pb tunnel create web --port 8080
  pb tunnel list
  pb tunnel --ephemeral 3000 --private
  pb tunnel stop preview_abc123`},
	"pb tunnel create": {
		Long: `Create a durable tunnel that serves a service on this machine. The machine must be enrolled
first (pb pair). Give the tunnel a name of 1 to 63 letters, digits, ".", "_" or "-",
not starting with punctuation, and its origin with --port (a local TCP port) or --from (an
origin URL: http, https, h2c, tcp or unix).

Access defaults to public. --private requires authenticated private access and --team requires an explicit team grant; they cannot be combined. --domain adds
a custom hostname after creation. By default the tunnel never expires; --duration sets a
lifetime, and pb tunnel extend moves it later.

` + waitNote,
		Example: `
  pb tunnel create web --port 8080
  pb tunnel create api --from http://127.0.0.1:9000 --private
  pb tunnel create docs --port 3000 --domain docs.example.com --wait
  pb tunnel create temp --port 8080 --duration 24h`},
	"pb tunnel list": {
		Long: `List durable tunnels you can see: ID, name, address, access mode and state. Filter with --q
(name or ID), --state and --owner (mine, shared or an authorized account ID); page with
--cursor and --limit (1-200, default 100). With --ephemeral the list shows temporary previews
instead, and --all adds ones that ended in the last 30 days.`,
		Example: `
  pb tunnel list
  pb tunnel list --state active --limit 50
  pb tunnel list --owner mine --json
  pb tunnel --ephemeral list --all`},
	"pb tunnel show": {
		Long: `Show one durable tunnel's identity, address, access mode and desired state. Name it by ID or
exact name; if a name is ambiguous, retry with the tunnel's ID. --json prints the whole
resource. For current health use pb tunnel status; for history use pb tunnel logs.`,
		Example: `
  pb tunnel show web
  pb tunnel show tunnel_abc123 --json`},
	"pb tunnel rename": {
		Long: `Rename a durable tunnel, selected by ID or current name. The name is only a label: the
tunnel's address, routes, connectors and access rules do not change.

` + waitNote,
		Example: `
  pb tunnel rename web website
  pb tunnel rename tunnel_abc123 web --wait`},
	"pb tunnel extend": {
		Long: `Move a durable tunnel's expiry to now plus --duration, for example 24h. Use it when
pb tunnel status says the tunnel expired; it serves again once the change applies.

` + waitNote,
		Example: `
  pb tunnel extend web --duration 720h
  pb tunnel extend tunnel_abc123 --duration 24h --wait`},
	"pb tunnel status": {
		Long: `Show current health for one durable tunnel, by ID or name. A route that is configured is not
necessarily ready to serve. --watch keeps refreshing every --interval (250ms to 1m, default 1s)
until interrupted. With --ephemeral it shows a temporary preview's status instead.`,
		Example: `
  pb tunnel status web
  pb tunnel status web --watch --interval 5s
  pb tunnel status web --json`},
	"pb tunnel doctor": {
		Long: `Diagnose where a tunnel is failing: placement, the machine's connection, the edge and the
origin. --bundle prints a preview of a bounded support bundle for the absolute path you give,
and writes nothing; add --write-bundle to write exactly the previewed bundle. --bundle-max-bytes
bounds its size (64KiB to 8MiB, default 2MiB).`,
		Example: `
  pb tunnel doctor web
  pb tunnel doctor web --bundle /tmp/bundle.json
  pb tunnel doctor web --bundle /tmp/bundle.json --write-bundle`},
	"pb tunnel logs": {
		Long: `Show recent events of a tunnel, 1 to 200 per page (--limit, default 100; --cursor continues).
--follow waits for new entries every --interval (250ms to 1m, default 1s). Logs explain what
happened; for the current state use pb tunnel status.`,
		Example: `
  pb tunnel logs web
  pb tunnel logs web --follow --interval 5s
  pb tunnel logs web --limit 50 --json`},
	"pb tunnel inspect": {
		Long: `Read or manage the HTTP requests this machine's daemon captured for a tunnel. Name the
route with --route when the tunnel has several; filter with --id, and page with --cursor and
--limit (1-100, default 20). --enable and --disable turn capturing on or off, --purge deletes
the captured requests, and --body and --raw (bodies, exact requests for replay) apply only
together with --enable.

Captures can contain sensitive traffic such as tokens and personal data. Treat the output
accordingly.`,
		Example: `
  pb tunnel inspect web --limit 10
  pb tunnel inspect web --route route_xyz --id cap_123
  pb tunnel inspect web --enable --body
  pb tunnel inspect web --purge`},
	"pb tunnel replay": {
		Long: `Send one captured HTTP request again to the same tunnel origin. This repeats whatever the
request did, including side effects such as payments or writes. Name the capture by its ID
(from pb tunnel inspect) and pass --route when the tunnel has several routes. An idempotency
key is generated unless you pass --idempotency-key; reuse one key to retry safely.`,
		Example: `
  pb tunnel replay web cap_123
  pb tunnel replay web cap_123 --route route_xyz --idempotency-key order-42`},
	"pb tunnel pause": {
		Long: `Stop new traffic to a durable tunnel while keeping its address, routes and domains.
Undo it with pb tunnel resume.

` + waitNote,
		Example: `
  pb tunnel pause web --wait`},
	"pb tunnel resume": {
		Long: `Let a paused durable tunnel carry new traffic again, with the same address and routes. If the
machine is offline or the origin is down it will still fail; check pb tunnel status.

` + waitNote,
		Example: `
  pb tunnel resume web --wait`},
	"pb tunnel delete": {
		Long: `Delete a durable tunnel with its endpoints, routes and domain bindings. DNS records you keep
at your own provider are not touched, and audit history stays. With --ephemeral, stop a
temporary preview instead.

` + confirmNote,
		Example: `
  pb tunnel delete web
  pb tunnel delete web --confirm A1B2C3 --json`},
	"pb tunnel stop": {
		Long: `Stop a temporary preview (the ephemeral lifecycle) by its preview ID. A durable tunnel's ID
(tunnel_...) is refused: use pb tunnel pause or pb tunnel delete for those.`,
		Example: `
  pb tunnel stop preview_abc123
  pb tunnel --ephemeral stop preview_abc123`},
	"pb tunnel route": {
		Long: `List, add, update or remove the routes of a durable tunnel. A route ties a protocol, a match
(a hostname and an optional path prefix) and an origin together, and a tunnel can have several.
Each change may return an operation that is still connecting; see pb tunnel status.`,
		Example: `
  pb tunnel route list web
  pb tunnel route add web --name app --to http://127.0.0.1:8080 --domain app.example.com`},
	"pb tunnel route add": {
		Long: `Add a route to a durable tunnel. --name and --to are required: --to is the origin URL the route forwards to. --domain is
the hostname it matches (an exact name or a one-label wildcard) and --path an optional HTTP path
prefix, which may end in *. --protocol is http (the default), tcp, tls passthrough or
tcp_private.

Origin options: --host-header or --preserve-host choose the Host the app sees; --tls-verification
(system, custom_ca, pinned or insecure) with --tls-ca, --tls-pin and --tls-server-name decide
how the origin's certificate is trusted; --connect-timeout (default 10s), --idle-timeout
(default 5m) and --max-streams (default 1024) bound the connection. Header, redirect and
path-rewrite rules are repeatable (--request-header and --response-header up to 8, --redirect up
to 4).

` + waitNote,
		Example: `
  pb tunnel route add web --name app --to http://127.0.0.1:8080 --domain app.example.com
  pb tunnel route add web --name db --protocol tcp --to tcp://127.0.0.1:5432 --domain db.example.com
  pb tunnel route add web --name docs --to http://127.0.0.1:3000 --domain app.example.com --path /docs --wait`},
	"pb tunnel route list": {
		Long: `List a durable tunnel's routes: ID, name, protocol, match, origin and state. Use the route ID
with route update, route remove, domain add and inspect. Page with --cursor and --limit.`,
		Example: `
  pb tunnel route list web
  pb tunnel route list web --limit 50 --json`},
	"pb tunnel route remove": {
		Long: `Remove one route from a durable tunnel. The tunnel and its other routes keep serving.

` + confirmNote,
		Example: `
  pb tunnel route remove web route_xyz
  pb tunnel route remove web route_xyz --confirm A1B2C3`},
	"pb tunnel route update": {
		Long: `Change the fields you name on one route; the rest stay as they are. The flags are the ones of
route add. --clear-* flags remove a single override (the Host override, path prefix, TLS server
name, header rules, redirects or rewrite), and --enable or --disable switches the route on or
off. Unlike add, update accepts only http and tcp_private for --protocol.

` + waitNote,
		Example: `
  pb tunnel route update web route_xyz --to http://127.0.0.1:8081
  pb tunnel route update web route_xyz --disable
  pb tunnel route update web route_xyz --clear-host-header --wait`},
	"pb tunnel domain add": {
		Long: `Serve one route of a tunnel at a hostname under a verified workspace domain: for example
app.example.com when example.com is added and verified with pb domain add and pb domain verify.
The domain's verification and certificate cover the hostname, so there is no DNS step of its
own. --route picks the route the hostname serves.

` + waitNote,
		Example: `
  pb tunnel domain add web app.example.com --route route_xyz
  pb tunnel domain add web app.example.com --route route_xyz --wait`},
	"pb tunnel domain list": {
		Long: `List the hostnames a durable tunnel is served at, with the certificate state of each. Add
hostnames with pb tunnel domain add and withdraw them with pb tunnel domain remove.`,
		Example: `
  pb tunnel domain list web
  pb tunnel domain list web --json`},
	"pb tunnel domain remove": {
		Long: `Stop serving a tunnel at a hostname. The workspace domain and its DNS records stay.

` + confirmNote,
		Example: `
  pb tunnel domain remove web app.example.com
  pb tunnel domain remove web app.example.com --confirm A1B2C3`},
	"pb tunnel policy": {
		Long: `Manage permission to open a private port on a machine when someone asks for it, without
making a tunnel first. A policy authorizes one exact machine port for private or team access. It
never publishes a public URL and never opens other ports of the machine. Revoking a policy
blocks later activations. If the application behind an approved port is replaced, the new one
inherits access; Paperboat does not start the application.`,
		Example: `
  pb tunnel policy allow homelab 3000
  pb tunnel policy list
  pb tunnel policy revoke pol_abc`},
	"pb tunnel policy allow": {
		Long: `Authorize on-demand access to one exact port of a machine: a bare port number or a loopback
http, https or h2c URL. --access chooses who may use it, private (the default) or team. The
policy lasts --expires (default 8h, must be positive). To change an existing policy pass its
--id together with the --generation you last saw. A replacement application on the same port
inherits the access.`,
		Example: `
  pb tunnel policy allow homelab 3000
  pb tunnel policy allow homelab https://127.0.0.1:8443 --access team --expires 1h
  pb tunnel policy allow homelab 3000 --id pol_abc --generation 3`},
	"pb tunnel policy get": {
		Long: `Show one on-demand port policy: its target, access mode, state, ID and generation. Use the ID
and generation to update it with pb tunnel policy allow or end it with pb tunnel policy revoke.`,
		Example: `
  pb tunnel policy get pol_abc
  pb tunnel policy get pol_abc --json`},
	"pb tunnel policy list": {
		Long: `List your on-demand port policies with their IDs, targets, generations and expiry. Expired
ones are included. Use it to find the ID of a policy you no longer remember.`,
		Example: `
  pb tunnel policy list
  pb tunnel policy list --json`},
	"pb tunnel policy revoke": {
		Long: `Revoke one on-demand port policy so later activations are denied. --generation is the
generation you expect; without it the command reads the current one first. Unrelated tunnel
routes are not affected.`,
		Example: `
  pb tunnel policy revoke pol_abc
  pb tunnel policy revoke pol_abc --generation 3`},
	"pb tunnel ip-rules": {Example: `
  pb tunnel ip-rules web
  pb tunnel ip-rules web --allow 203.0.113.0/24 --deny 203.0.113.7
  pb tunnel ip-rules web --clear`},
	"pb preview": {
		Long: `Expose a local target through a temporary preview URL. The target is a bare port (3000
means http://127.0.0.1:3000), an http, https, h2c or tcp URL with an explicit port and nothing
else (no path, query or user), or an absolute Unix socket path (also written unix:///path).

A preview is public by default. --private limits it to your account and --team requires an
explicit team grant (they cannot be combined). --ttl limits its lifetime to at most 24h.
--host-header sends a different Host to the origin, for servers that allow-list one (http, https
and h2c targets; give a bare name without a port). --domain attaches a verified custom domain
and waits up to 15 minutes for its DNS and certificate.

The preview runs while the command runs; Ctrl+C stops it. --background hands it to this
computer's background service and returns once it is ready, with a default lifetime of 30
minutes.`,
		Example: `
  pb preview 3000
  pb preview https://127.0.0.1:8443 --private --ttl 2h
  pb preview unix:///run/app.sock --host-header myapp.test
  pb preview 3000 --background --json`},
	"pb preview list": {
		Long: `List previews: ID, access, target and state. Only active previews are shown by default;
--all adds previews that ended in the last 30 days, and --state picks one state. Filter with
--q and --owner; page with --cursor and --limit (1-200, default 100). Use a preview's ID with
status, inspect, replay, stop or delete.`,
		Example: `
  pb preview list
  pb preview list --all
  pb preview list --state active --json`},
	"pb preview status": {
		Long: `Show one preview's access, target, state and deadline. A preview that exists is not
necessarily reachable: if nothing answers on the target yet, visitors see "App not responding"
until your app starts.`,
		Example: `
  pb preview status preview_abc123
  pb preview status preview_abc123 --json`},
	"pb preview stop": {
		Long: `Stop one temporary preview, selected by its ID or name, and withdraw its published access. Other
previews and durable tunnels are not affected. The command prints the stopped preview, or with
--json the stopped resource. pb preview delete does the same.`,
		Example: `
  pb preview stop preview_abc123
  pb preview stop preview_abc123 --json`},
	"pb preview delete": {Example: `
  pb preview delete preview_abc123`},
	"pb preview inspect": {
		Long: `Read or manage the HTTP requests this machine's daemon captured for a preview. Filter with
--id, and page with --cursor and --limit (1-100, default 20). --enable and --disable turn
capturing on or off, --purge deletes the captured requests, and --body and --raw (bodies, exact
requests for replay) apply only together with --enable. A preview has one origin, so unlike
pb tunnel inspect there is no --route.

Captures can contain sensitive traffic such as tokens and personal data. Treat the output
accordingly.`,
		Example: `
  pb preview inspect preview_abc123 --limit 10
  pb preview inspect preview_abc123 --enable --body
  pb preview inspect preview_abc123 --id cap_123 --json`},
	"pb preview replay": {
		Long: `Send one captured HTTP request again to the preview's origin, by capture ID (from
pb preview inspect). This repeats whatever the request did, including side effects. Pass
--idempotency-key when the origin supports one, to retry safely. A preview has one origin, so
unlike pb tunnel replay there is no --route.`,
		Example: `
  pb preview replay preview_abc123 cap_456
  pb preview replay preview_abc123 cap_456 --idempotency-key order-42`},
	"pb preview ip-rules": {Example: `
  pb preview ip-rules preview_abc123
  pb preview ip-rules preview_abc123 --allow 203.0.113.0/24
  pb preview ip-rules preview_abc123 --clear`},
}
