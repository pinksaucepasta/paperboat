module github.com/pinksaucepasta/paperboat/sdk/go

go 1.27.4

require github.com/pinksaucepasta/paperboat/shared v0.0.0

require (
	github.com/google/uuid v1.6.0 // indirect
	golang.org/x/net v0.62.0 // indirect
	golang.org/x/sys v0.50.0 // indirect
	golang.org/x/text v0.43.0 // indirect
)

replace github.com/pinksaucepasta/paperboat/shared => ../../shared
