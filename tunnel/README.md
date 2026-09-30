# paperboat-tunnel

Paperboat's public and private edge. The service owns authenticated connector-v1
carriers, route reconciliation, native TLS HTTP/1.1 and HTTP/2 ingress, HTTP/3
ingress, certificate selection, usage accounting, and node lifecycle.

The edge accepts only server-admitted connectors. HTTPS and TLS pass-through share
public TCP 443: HTTPS terminates at the edge, while pass-through preserves the origin
certificate and encrypted bytes. Each hostname has one mode. Private HTTP arrives over
an authorized connector stream and is sent to the private TLS listener. The infrastructure
hostname serves health and authenticated installation verification.
Plain HTTP redirects permanently to HTTPS.

## Development

```sh
make check
```

The workspace module includes the shared `paperboat-relay/selfhost` runtime-file
reader. Build from the Paperboat OSS workspace so both modules are available.

Production configuration is illustrated by `deploy/deployment.example.json`.
The container contains one executable, `paperboat-tunnel`; there are no external
proxy or signaling binaries to provision.

For native relay-only, tunnel-only, or combined installation, see
[self-host installation](../relay/deploy/README.md). The `pbh` helper generates the
strict deployment document after an authenticated dashboard claim. Installation
and claim need no DNS domains; configured browser/public routes need domain and
certificate readiness before publication. The checked-in JSON is a field and path
example for an already configured node.

## License

MIT. See [LICENSE](LICENSE).
