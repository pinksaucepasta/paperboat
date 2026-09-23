# Paperboat public components

This source tree contains three Go modules:

| Directory | Binary | Purpose |
| --- | --- | --- |
| `cli/` | `pb` | CLI and endpoint daemon (`pb daemon`) |
| `relay/` | `paperboat-relay` | Regional peer relay service and the relay packages compiled into `pb` |
| `tunnel/` | `paperboat-tunnel` | Browser and public tunnel edge |

Use Go 1.27.1. The checked-in `go.work` binds the three modules, so the CLI
compiles `relay/` packages directly. The running relay service is a separate
deployment; it is not needed to compile `pb`. The only patched networking
dependency is the public, revision-pinned Tailscale fork in the CLI and relay
module files.
Build and test from this checkout with workspace mode enabled. The component
modules do not support standalone `GOWORK=off` builds while their relay import
has no published module version.

From the repository root, run `make build` for the three Linux host binaries
in ignored `.build/`, or `make test` for focused component checks. Module
commands also work from their component directories with this workspace.

Build either service image with the **repository root** as Docker context:

```sh
docker build -f relay/deploy/Dockerfile --build-arg PAPERBOAT_VERSION=2026.09.23.0 .
docker build -f tunnel/deploy/Dockerfile --build-arg PAPERBOAT_VERSION=2026.09.23.0 .
```

The checked-in component contract fixtures are local test inputs. Cross-project
schema ownership and generated consumers are verified in the later contract
gate. This tree is a source candidate; no release or deployment is implied.
