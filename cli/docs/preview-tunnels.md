# Previews And Tunnels

Paperboat has one tunnel workflow with two lifecycle modes. An ephemeral tunnel
(`pb preview` or `pb tunnel --ephemeral`) is temporary and normally owned by the
foreground session that created it. A durable tunnel survives connector,
network, service, and host restarts. Both keep one stable endpoint while their
live connector or edge assignment changes.

## Start a preview

Expose a local port, URL, or directory:

```console
pb preview 3000
pb preview http://127.0.0.1:8080
pb preview ./dist
pb tunnel --ephemeral 3000
```

Human output shows preparation immediately. The preview is published even when
nothing answers on the port yet, as from the console: the command prints
"Nothing answers on port N yet; the preview shows 'App not responding' until your
app starts", the address answers "App not responding", and it serves the app
within seconds of the app starting. Only invalid input (a bad port or target, a
preview the account may not create) is refused. It waits for authenticated edge
readiness before printing the managed URL. A readiness timeout includes the last
observed allocation, edge, and origin states. `--json` omits progress messages.
Keep it running in the foreground. Ctrl+C stops the owner session and
withdraws the preview. A preview is never restored after reboot.

Set an optional maximum lifetime, request account-private access, or attach
custom domains:

```console
pb preview 3000 --ttl 2h
pb preview 3000 --background --ttl 1h
pb preview 3000 --private
pb preview 3000 --domain demo.example.com
pb preview 3000 --json
```

Background mode transfers ownership to the running daemon before returning. It
defaults to 30 minutes and is capped at 24 hours. It keeps the same URL through a restart of
the daemon or the host service (an update restarts it), and is not restored after reboot.
`--duration` remains a deprecated spelling of `--ttl`. `--domain` is repeatable.
The managed Paperboat URL becomes usable as soon as
the preview is ready; a custom alias must be under a verified workspace domain (`pb domain add`) and
is ready once its certificate copy reaches the edges. Wildcards match one label only.

List or stop previews:

```console
pb preview list
pb preview list --json
pb preview status <preview>
pb preview stop <preview>
pb preview delete <preview>
```

## Private previews

Private browser traffic follows one path:

```text
browser -> hostd-owned PAC/system proxy -> authenticated carrier -> edge -> origin
```

The stable Paperboat host runtime installs and owns a narrow local proxy rule
for Paperboat private hostnames. Untrusted network PAC or WPAD remains rejected.
The browser sends no Paperboat cookie, access token, proof header, redirect, or
login callback. If the local runtime is not installed, running, signed in, or
authorized, the private URL cannot work.

Private failures are deliberate: `401 Unauthorized` means the current machine
session is missing or invalid, `403 Forbidden` means the authenticated machine
is denied or revoked for that route, and `503 Service Unavailable` means hostd,
the carrier, edge binding, or authorization authority is temporarily
unavailable. Responses do not reveal another account's resource.

## Create a durable tunnel

Create from a port or origin URL. `--wait` waits for the operation to become
terminal and `--json` returns the canonical resource projection.

```console
pb tunnel create api --port 8080 --wait
pb tunnel create api --from https://127.0.0.1:8443 --private --wait
pb tunnel create api --port 8080 --domain api.example.com --wait --json
```

A tunnel's public address is generated for you from the name generator (three
readable words and four digits under the deployment's public base domain, for example
`calm-summer-beach-3476.pprbt.app`). It is stable for the life of the tunnel and never
derives from the name you type. The name is only a label: `pb tunnel rename api web-api`
changes it without touching the address, names need not be unique, and `pb tunnel list`
and `pb tunnel show` print the address next to the name. A preview's name cannot be
changed. To serve from your own address, attach a custom domain (below).

Inspect the durable resource and live health:

```console
pb tunnel list
pb tunnel show api
pb tunnel status api
pb tunnel status api --watch
pb tunnel logs api --follow
```

When a tunnel is not serving, `pb tunnel status`, `pb tunnel logs` and `pb tunnel doctor`
name the failing step and what to do about it; `--json` carries the code in
`overall_code`. For an unreachable app `overall_code` is `origin_unreachable` and the reason
(`origin_refused`, `origin_timeout`, `origin_unreachable` or `origin_tls`) is
`dimensions.origin.code`. The codes are:

| Code | Meaning | What to do |
|---|---|---|
| `connector_offline` | The host that runs the tunnel is off or its service is stopped | Make sure the machine is on (`pb environments`) and run `pb doctor` on it; the tunnel reconnects by itself |
| `edge_unavailable` | The host is connected but no edge is ready | Nothing to change; if it lasts several minutes run `pb tunnel doctor` and send the support reference |
| `dns_pending` | The hostname's domain is not verified yet | Add the records from `pb domain show <domain>`, then `pb domain verify <domain>` |
| `certificate_pending` | The certificate is still being issued | Wait; issuance starts after DNS is verified (`pb tunnel domain list`) |
| `origin_refused` | The app is not listening on the route's port | Start it, or fix the port with `pb tunnel route update`; the tunnel recovers within seconds |
| `origin_timeout` | The app is running but did not answer in time | Check that it is healthy and not behind a firewall |
| `origin_unreachable` | The host cannot reach the route's address | Check the address with `pb tunnel route list` |
| `origin_tls` | The app's TLS certificate was not accepted | Trust it with the route's custom CA setting, or use an http origin |
| `route_unavailable` | The edge rejected the route | Fix the origin address with `pb tunnel route update` |
| `bandwidth_exhausted` | The workspace has used its bandwidth allowance, so traffic is not forwarded | Add bandwidth or change plan on the Billing page; traffic resumes when the allowance is available |

An empty `pb tunnel logs` is not left unexplained: it prints the tunnel's current state and
what to do about it.

Pausing withdraws new traffic without deleting the tunnel. Resuming reconciles
the saved desired state. Deleting is an explicit destructive operation.

```console
pb tunnel pause api --wait
pb tunnel resume api --wait
pb tunnel delete api --wait
```

Deleting and other destructive tunnel changes first show their effect and a confirmation
command. Run the displayed command within five minutes to proceed.

## Routes and origins

A tunnel can have multiple HTTP, public TCP, or private TCP routes:

```console
pb tunnel route add api --name web --to http://127.0.0.1:3000 --domain api.example.com --path / --wait
pb tunnel route add api --name database --to 127.0.0.1:5432 --protocol tcp --wait
pb tunnel route add api --name database --to 127.0.0.1:5432 --protocol tcp_private --wait
pb tunnel route list api
```

HTTP routes preserve the public `Host` by default. Origin host-header rewrite,
TLS server name, verification mode, trusted CA, client credential reference,
timeouts, priority, and stream limits are independent route settings.

`--tls-ca <file>` trusts one CA for the route (the PEM file is checked and
stored with the route; only that CA is trusted, not the system roots).

An https origin on `localhost` or an IP address is sent the public hostname as
TLS server name, so Caddy, Traefik and nginx pick the right site. For an origin
with a self-signed certificate (a NAS, Proxmox), `--tls-verification pinned`
shows the certificate it presents and pins it; `--tls-pin sha256:<hex>` pins it
without asking. If the certificate changes, the route is refused until you
re-pin it. `--tls-verification insecure` turns checks off; for an origin that
is not on the machine itself, tunnel status warns while it is set.

Change or remove a route with `pb tunnel route update api web ...` and
`pb tunnel route remove api web --wait`; the tunnel stays.

Exact hosts beat one-label wildcards. Within one host, the longest path prefix
then route priority wins. A public `tcp` route reserves a stable managed-hostname
port and carries opaque bytes; the application supplies its own authentication and
optional TLS. It has no HTTP path and does not use Paperboat's private-access helper.
A `tcp_private` route has no public listener or HTTP hostname/path matcher and never
enters the public route table.

### Header and URL rules on a route

An http route can carry a few rules that the Paperboat edge applies before a request reaches your app: request and response
header changes, one path-prefix rewrite and redirects. They are stored with the route and travel with it.

```sh
pb tunnel route update api web \
  --request-header 'set:X-Env=prod' --request-header 'remove:X-Debug' \
  --response-header 'add:X-Served-By=paperboat' \
  --rewrite-path '/api -> /v1' \
  --redirect '/old -> 308:/new' --redirect '/docs -> 301:https://example.com/help'
```

- Headers: `set` replaces, `add` appends, `remove` deletes; at most 8 per direction, names of letters, digits and `-_.`,
  values up to 4096 bytes without line breaks.
- `--rewrite-path '/from -> /to'` changes the path the app sees; it matches whole segments (`/api` covers `/api/x`, not `/apix`)
  and does not apply to WebSocket handshakes.
- `--redirect '/from -> [301|302|307|308:]target'` answers at the edge without calling your app (302 when no status is given);
  the target is a path or an `https://` URL, what follows the prefix and the query are kept; at most 4, longest prefix first.
- The edge keeps what it owns: rules cannot name `Forwarded`, `X-Forwarded-*`, `X-Real-IP`, `Host`, `Cookie`, `Set-Cookie`,
  `Authorization`, `X-Paperboat-*`, `Paperboat-*` (including the signed visitor assertion), connection headers, `Alt-Svc` or the
  security headers it sets (use `--host-header` for the Host the app sees).
- On update each list is replaced as a whole and the route's other rules stay; `--clear-request-headers`,
  `--clear-response-headers`, `--clear-rewrite-path` and `--clear-redirects` remove one. Previews and TCP/TLS routes take no rules.

## Apps and proxies behind a tunnel

Paperboat serves the public name over HTTPS and tells the origin so. Every
request carries headers the edge sets itself (anything the visitor sent under
these names is dropped first): `X-Forwarded-Proto: https`, `X-Forwarded-Host`
(the public name), `X-Forwarded-Port: 443`, `X-Forwarded-For` and `X-Real-IP`
(the visitor's address, one value), and `Forwarded`. The request arrives from
loopback, which most frameworks already trust, so redirects, absolute URLs,
`Secure` cookies and CSRF origin checks work without changes.

When a reverse proxy sits in front of your app, it decides what the app sees:

| Proxy | Settings |
|---|---|
| nginx | `proxy_set_header X-Forwarded-Proto $http_x_forwarded_proto;` (not `$scheme`, which says `http`); `set_real_ip_from 127.0.0.1; real_ip_header X-Forwarded-For;` for the visitor's address; `proxy_buffering off;` so streamed responses (SSE, AI tokens) are not held back; `client_max_body_size` at least your upload size (nginx refuses bodies over 1 MiB by default) |
| Caddy | `trusted_proxies static 127.0.0.1/32 ::1/128` in the global `servers` block; serve the site as `http://name` or use `auto_https disable_redirects` on the port the tunnel uses |
| Traefik | `entryPoints.<name>.forwardedHeaders.trustedIPs=127.0.0.1/32,::1/128` (add the Docker bridge if Traefik runs in a container); put the HTTP-to-HTTPS redirect only on entrypoints the tunnel does not use |
| Apache | `RemoteIPHeader X-Forwarded-For` with `RemoteIPInternalProxy 127.0.0.1`; `SetEnvIf X-Forwarded-Proto https HTTPS=on` and guard `RewriteCond %{HTTPS} off` redirects with it; `Proxy100Continue Off` when the app behind it does not answer `Expect: 100-continue` (HTTP/1.0 servers), or uploads fail with 503 |

On a team or private route the app also learns who is visiting:
`Paperboat-Visitor-Assertion` carries a JWT (`typ` `paperboat-visitor+jwt`,
EdDSA) signed by the Paperboat server, with `sub` (the visitor's account),
`email`, `team_id` for team routes, `aud` (the route's public hostname), and a
lifetime of at most five minutes. Verify it against the keys the server
publishes at `/.well-known/jwks.json` on its API address, and check `aud` and
`exp`. Public routes never carry it, and a visitor cannot send their own.

Without the trusted-proxy line the proxy replaces the visitor's address with
its own, so the app sees every visitor as one address; scheme and host still
arrive. If the proxy's own port 80 redirects to HTTPS, point the route at the
proxy's HTTPS port instead (`--to https://localhost:443`).

Your origin does not need a public certificate: Paperboat holds the certificate
for the public name. Behind a tunnel the origin cannot get one by HTTP-01 or
TLS-ALPN-01 either, because Paperboat answers for the name (an ACME request for
it gets a 404 that says so). Pick one:

| Origin | Route |
|---|---|
| Plain HTTP (simplest) | `--to http://localhost:80` |
| Caddy `tls internal`, or an origin with its own private CA | `--to https://localhost:443 --tls-ca <root.crt>`: trust Caddy's root, which Caddy logs at startup (in Docker `/data/caddy/pki/authorities/local/root.crt`). Do not pin it: Caddy renews its internal certificate every 12 hours with a new key |
| A long-lived self-signed certificate (NAS, Proxmox, router) | `--tls-verification pinned` |
| Any local certificate, when you just want it to work | `--tls-verification insecure`, only for an origin on the same machine; for any other origin tunnel status warns |
| A real certificate from a DNS-01 challenge | the default verification |

## Custom domains, DNS, and TLS

Add a domain you control to the workspace once, add the DNS records it prints,
and wait until it is verified:

```console
pb domain add dev.example.com
pb domain verify dev.example.com
pb domain list
```

The `_acme-challenge` CNAME proves you control the domain and lets Paperboat
issue one certificate for the domain and every name one level under it. The
`*.dev.example.com` record sends those names to Paperboat. After that, any
tunnel or preview can use a name under the domain with no DNS step of its own:

```console
pb tunnel domain add api api.dev.example.com --route web
pb tunnel domain list api
pb tunnel domain remove api api.dev.example.com --wait
pb domain remove dev.example.com
```

A domain in use cannot be removed; `pb domain show` lists what uses it. Team
domains are added and removed by team owners and admins.

Paperboat keeps the previous valid certificate active until ownership,
issuance, and distribution to every captured edge generation succeed. Removing
a binding never deletes user-owned DNS records.

## Private TCP

Open a literal-loopback listener through the stable runtime:

```console
pb access tunnel api --listen 127.0.0.1:0
pb access tunnel database --listen 127.0.0.1:0 --json
```

The listener accepts only local connections. Each accepted connection obtains
a fresh short-lived grant and opens a native peer stream bound to the current
machine access session and exact route and target generations. The host checks
current desired state before dialing the exact origin; pause, deletion,
revocation, expiry, or a generation change closes active access. Closing the
command closes the local listener; it does not change the durable route.

Native private HTTP keeps the existing loopback PAC/CONNECT experience but
uses a dedicated preview-class Paperboat QUIC session. The session is admitted
for the current machine-access grant and then owned exclusively by HTTP/3; the
CONNECT request carries the exact signed preview/tunnel route and target
binding. Paperboat revalidates current state before dialing the literal-loopback
origin. A broken request is not replayed: the next browser connection obtains a
fresh grant and session. This native path never enters `paperboat-edge` and
does not silently fall back to the edge-decrypted browser path.

## Diagnostics and support

Run bounded diagnostics before changing state:

```console
pb tunnel doctor api
pb tunnel doctor api --json
```

The result covers service, connector, generation, origin, DNS, certificate,
private access, update, and rollback health without returning credentials.
Preview the support-bundle manifest before upload and remove any unexpected
private hostname, path, header, payload, or local address.

For recovery procedures, see
[`runbooks-preview-tunnels.md`](runbooks-preview-tunnels.md).

### On-demand private/team ports

Approve a port once, then use its reserved URL while the policy is valid:

```sh
pb tunnel policy allow <machine-id> 3000 --expires 8h
pb tunnel policy get <policy-id>
pb tunnel policy allow <machine-id> 3000 --id <policy-id> --generation 1 --expires 24h
pb tunnel policy revoke <policy-id> --generation 2
```

Sharing follows replacement applications at the same authorized port. Paperboat
connects to an already-running service; it does not start an application or wake
the machine. Use `--access team` only with an owner-issued team policy binding and
resource grant. Default access is private. Commands support `--json`.

Concurrent authorized visits share one activation. Idle forwarding closes after
five minutes without requests or streams, while the reserved name remains. Each
activation lasts at most eight hours, shortened by policy and authorization
expiry; streams last at most one hour. Revocation ends permission and permanently
tombstones the name. Re-enrollment requires explicit owner reapproval.

Production browser access remains subject to the managed hostname isolation gate;
reserving a policy does not bypass that requirement.
