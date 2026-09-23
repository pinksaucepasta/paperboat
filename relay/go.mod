module github.com/pinksaucepasta/paperboat-relay

go 1.27.1

require (
	github.com/coder/websocket v1.8.14
	github.com/getsentry/sentry-go v0.49.0
	github.com/quic-go/quic-go v0.61.0
	go4.org/mem v0.0.0-20240501181205-ae6ca9944745
	tailscale.com v1.103.0-pre.0.20260904030409-31d8badb3bfb
)

require (
	github.com/go-json-experiment/json v0.0.0-20260623181947-01eb4420fa68 // indirect
	github.com/golang/groupcache v0.0.0-20241129210726-2c02b8208cf8 // indirect
	github.com/jsimonetti/rtnetlink v1.4.1 // indirect
	github.com/mdlayher/netlink v1.7.3-0.20250113171957-fbb4dce95f42 // indirect
	github.com/mdlayher/socket v0.5.1 // indirect
	go4.org/netipx v0.0.0-20260823151212-3075585bcbeb // indirect
	golang.org/x/crypto v0.55.0 // indirect
	golang.org/x/exp v0.0.0-20260410095643-746e56fc9e2f // indirect
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.41.0 // indirect
	golang.org/x/time v0.15.0 // indirect
)

replace tailscale.com => github.com/pinksaucepasta/tailscale v1.103.0-pre.0.20260923014721-77d76f6fe81c
