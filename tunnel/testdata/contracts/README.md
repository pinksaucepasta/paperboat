# Repository-local contract fixtures

This directory contains JSON, NDJSON, and JSON Schema fixtures owned by
`paperboat-tunnel` and consumed by its tests. Keep fixtures beside the tests
that use them and preserve the protocol and security behavior coverage.

Run affected checks from the repository root, including:

```sh
go test ./internal/contracttest
```

Contracts that other modules read too (credentials, helper frames, sessions, edge control, p2p,
preview-tunnel, telemetry, environment E2EE) have their one copy in `shared/testdata/contracts`.
