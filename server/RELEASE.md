# Service container releases

`paperboat-server` is released only as the Linux amd64/arm64 image
`ghcr.io/pinksaucepasta/paperboat/server`. Its GHCR visibility and access controls
must remain private. Relay and tunnel image publication does not authorize any
change to the server package visibility.

Use `tools/release-version.sh next` to select a `YYYY.MM.DD.X` tag. Daily sequence
numbers start at zero and are contiguous, and the tagged commit must be contained
in `main`. The common service workflow refuses to overwrite a version tag,
publishes that immutable version and moves `latest`, attaches OCI source/revision/
version labels, produces SBOM and provenance attestations, and reports the
multi-architecture manifest digest.

Pin production and rollback configuration to the reported digest:

```text
ghcr.io/pinksaucepasta/paperboat/server@sha256:<manifest-digest>
```

When Sentry release association is configured in GitHub, the publish workflow
creates `paperboat-server:<tag>`. A deployment that enables Sentry must set
`PAPERBOAT_SENTRY_RELEASE` to that exact value; the image tag, source commit, and
runtime telemetry then share one release identity. Image publication does not
create a Sentry deploy record.

Before the first unified release, the public relay repository must first contain
the shared workflow on `main`; then replace this caller's bootstrap `@main`
reference with that real commit SHA. Pulling a private server digest requires a
GHCR principal with package read access. Preserve the prior digest, run migrations
under the existing deployment procedure, check `/healthz` and `/readyz`, and roll
back to the recorded digest if the service does not become ready. Database schema
rollback remains governed by the forward-only migration contract.
