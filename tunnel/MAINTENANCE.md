# Maintenance

Paperboat Tunnel is a native Go edge. Keep the pinned `quic-go` and
`go-realclientip` revisions aligned with `go.mod`, security review their release
notes before upgrades, and run the focused runtime, HTTP ingress, carrier, and control
tests.

Build and publish only `paperboat-tunnel`. Verify TLS HTTP/1.1, HTTP/2 and HTTP/3,
redirect behavior, certificate selection, connector admission, private access, drain,
and shutdown before release.
