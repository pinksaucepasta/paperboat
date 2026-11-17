# Changelog

Release notes for Paperboat, newest first. Releases are tagged `YYYY.MM.DD.X`; the client, server,
relay and edge are released together. This file starts at the launch release; earlier builds were
pre-release and are not listed.

## Launch release (version to be filled in at release)

First public release.

- **Machines:** `pb` for Linux (amd64, arm64), macOS (Apple Silicon) and Windows (amd64, arm64), installed
  with one command and updated automatically from signed release metadata.
- **Terminals and commands:** terminals that survive disconnects and reconnect with replay, `pb exec`,
  managed `pb ssh`, `scp`, `sftp` and `rsync`, all end-to-end encrypted, peer-to-peer where the network
  allows and through a relay where it does not.
- **Files:** resumable transfers between machines and a per-machine Inbox.
- **Previews and tunnels:** `pb preview` links, durable tunnels with HTTP, public TCP, TLS pass-through and
  private TCP routes, custom domains, and a request inspector.
- **ENV:** end-to-end encrypted environment variables with personal and team vaults and recovery codes.
- **Teams:** roles, access policy, API tokens, config sync, audit log, and SSO and SCIM on the Team plan.
- **Web console** for machines, sessions, previews, tunnels, ENV and teams.
- **Self-hosting:** run the relay and the preview edge on your own server with `pbh`, or run a Paperboat
  machine in a container.
