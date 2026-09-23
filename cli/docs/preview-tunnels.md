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

The command waits for origin and edge readiness before printing the managed
URL. Keep it running in the foreground. Ctrl+C stops the owner session and
withdraws the preview. A preview is never restored after reboot.

Set an optional maximum lifetime, request account-private access, or attach
custom domains:

```console
pb preview 3000 --ttl 2h
pb preview 3000 --background --ttl 1h
pb preview 3000 --private
pb preview 3000 --domain demo.example.com --domain '*.apps.example.com'
pb preview 3000 --json
```

Background mode transfers ownership to the running daemon before returning. It
defaults to 30 minutes, is capped at 24 hours, and is not restored after reboot.
`--duration` remains a deprecated spelling of `--ttl`. `--domain` is repeatable.
The managed Paperboat URL becomes usable as soon as
the preview is ready; a custom alias remains pending until its ownership, DNS,
and certificate checks finish. Wildcards match one label only.

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

Inspect the durable resource and live health:

```console
pb tunnel list
pb tunnel show api
pb tunnel status api
pb tunnel status api --watch
pb tunnel logs api --follow
```

Pausing withdraws new traffic without deleting the tunnel. Resuming reconciles
the saved desired state. Deleting is an explicit destructive operation.

```console
pb tunnel pause api --wait
pb tunnel resume api --wait
pb tunnel delete api --yes --wait
```

## Routes and origins

A tunnel can have multiple HTTP, public TCP, or private TCP routes:

```console
pb tunnel route add api --name web --to http://127.0.0.1:3000 --domain api.example.com --path / --wait
pb tunnel route add api --name database --to 127.0.0.1:5432 --protocol tcp --wait
pb tunnel route add api --name database --to 127.0.0.1:5432 --protocol tcp_private --wait
pb tunnel route list api
```

HTTP routes preserve the public `Host` by default. Origin host-header rewrite,
TLS server name, verification mode, CA reference, client credential reference,
timeouts, priority, and stream limits are independent route settings. Prefer
the defaults; use `--tls-verification insecure` only with an explicit reviewed
exception.

Exact hosts beat one-label wildcards. Within one host, the longest path prefix
then route priority wins. A public `tcp` route reserves a stable managed-hostname
port and carries opaque bytes; the application supplies its own authentication and
optional TLS. It has no HTTP path and does not use Paperboat's private-access helper.
A `tcp_private` route has no public listener or HTTP hostname/path matcher and never
enters the public route table.

## Custom domains, DNS, and TLS

Add, inspect, verify, or remove a domain binding:

```console
pb tunnel domain add api api.example.com --route web
pb tunnel domain instructions api api.example.com --json
pb tunnel domain verify api api.example.com --wait --timeout 10m
pb tunnel domain list api
pb tunnel domain remove api api.example.com --yes --wait
```

Apply only the provider-aware instructions returned by Paperboat. For a
wildcard, create the exact CNAME delegation shown for `_acme-challenge`; do not
copy certificate or DNS-provider credentials to an edge. Coolify can then
create previously unknown one-label application hosts beneath the verified
wildcard without another Paperboat route change.

Paperboat keeps the previous valid certificate active until ownership,
issuance, and distribution to every captured edge generation succeed. Removing
a binding never deletes user-owned DNS records.

## Connectors and credential rotation

Add and inspect connectors without copying enrollment secrets:

```console
pb tunnel connector add api --json
pb tunnel connector list api
```

Make a replacement ready before draining the old connector:

```console
pb tunnel connector drain api <connector> --wait --timeout 10m
pb tunnel credentials rotate api --yes --wait --timeout 10m
pb tunnel connector revoke api <connector> --yes --wait --timeout 10m
```

Rotation captures an immutable target set, proves the new key for each target,
keeps the old generation only for a bounded overlap, and completes after every
target is ready. Revocation is for compromise or permanent retirement; it does
not delete the tunnel, routes, domains, or certificates.

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
fresh grant and session. This native path never enters `paperboat-tunnel` and
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
