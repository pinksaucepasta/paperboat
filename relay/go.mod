module github.com/pinksaucepasta/paperboat-relay

go 1.27.1

require (
	github.com/coder/websocket v1.8.15
	github.com/getsentry/sentry-go v0.49.0
	github.com/quic-go/quic-go v0.61.0
	go4.org/mem v0.0.0-20240501181205-ae6ca9944745
	golang.org/x/sys v0.48.0
	tailscale.com v1.103.0-pre.0.20260923014604-610b05c58e8d
)

require (
	github.com/dblohm7/wingoes v0.0.0-20260526185140-fb298caac7ca // indirect
	github.com/go-json-experiment/json v0.0.0-20260820222146-c27c302e5fc3 // indirect
	github.com/golang/groupcache v0.0.0-20241129210726-2c02b8208cf8 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/jsimonetti/rtnetlink v1.4.2 // indirect
	github.com/mdlayher/netlink v1.11.2 // indirect
	github.com/mdlayher/socket v0.7.0 // indirect
	go4.org/netipx v0.0.0-20260823151212-3075585bcbeb // indirect
	golang.org/x/crypto v0.57.0 // indirect
	golang.org/x/exp v0.0.0-20260908205506-85c1c2202aba // indirect
	golang.org/x/net v0.59.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/text v0.42.0 // indirect
	golang.org/x/time v0.16.0 // indirect
	golang.zx2c4.com/wireguard/windows v1.0.1 // indirect
)

replace tailscale.com => github.com/pinksaucepasta/tailscale v1.103.0-pre.0.20260928002927-8c8b2e0dc313
