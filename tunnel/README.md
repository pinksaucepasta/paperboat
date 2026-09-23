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

Dependencies are checked in through standard `go mod vendor` output. CI and Docker
build from that snapshot, including the shared `paperboat-relay/operator` package;
a sibling checkout is not required. When changing that shared package, regenerate
with `go mod vendor` from the reviewed sibling checkout and review the vendor diff.
The local `replace` directive is used only when refreshing dependencies.

Production configuration is illustrated by `deploy/deployment.example.json`.
The container contains one executable, `paperboat-tunnel`; there are no external
proxy or signaling binaries to provision.

For relay-only, tunnel-only, and combined operator-managed Docker layouts, see
[OPERATOR.md](OPERATOR.md). Operator setup generates the actual strict deployment
document; the checked-in JSON is a field and path example.

## License

MIT. See [LICENSE](LICENSE).
