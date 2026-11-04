module github.com/pinksaucepasta/paperboat/sdk/go

go 1.27.3

require github.com/pinksaucepasta/paperboat/shared v0.0.0

require (
	github.com/google/uuid v1.6.0 // indirect
	golang.org/x/net v0.60.0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	golang.org/x/text v0.42.0 // indirect
)

replace github.com/pinksaucepasta/paperboat/shared => ../../shared
