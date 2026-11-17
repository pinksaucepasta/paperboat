# How Paperboat protects your machines

This page says what Paperboat protects, what its servers can see, and what it cannot protect. It is the
short version for users; [threat-model.md](threat-model.md) is the detailed one for people who review the
design, and [SECURITY.md](../../SECURITY.md) says how to report a vulnerability.

## Who can read what

| What | Who can read it |
|---|---|
| Terminals, remote commands, SSH, file transfers, private previews | The two machines. The content is end-to-end encrypted between the `pb` endpoint that starts it and the machine it reaches. A relay, when one is used, forwards ciphertext only. |
| Terminals in the web console | Your browser and the machine; the encryption runs in the browser. |
| ENV values | Your own devices and the machines you send them to. Values are encrypted on your device before upload. |
| Public previews and tunnels | The visitor, the Paperboat edge and your machine. HTTPS ends at the edge, which reads each request to route it, then forwards it to your machine encrypted. This is **not** end-to-end. A `tls` route is the exception: the edge forwards the encrypted bytes without ending TLS. |
| Config sync files | Whoever can read your Git repository. Paperboat adds no encryption there. |

## What the servers see

To authorize, route and bill, the control plane and the edges handle metadata only: account, machine and
endpoint identifiers, public keys and certificates, which carrier or relay region a connection used,
timestamps, ciphertext sizes and counters, health states and failure codes. They are not given commands,
file names or paths, terminal output, SSH traffic, preview bodies, ENV values, or any private key.

A relay or a network observer can still see that two addresses are talking, when, and roughly how much.
Encryption does not hide that.

## Connections do not get weaker

Before a terminal or file connection opens, `pb` checks the machine's descriptor: its scheme, issuer,
account, endpoint, generation and expiry. The machines verify each other's certificates before traffic
flows, so a relay or the control plane cannot sit in the middle of a session.

If the preferred path is unavailable, `pb` may use another path with the same encryption. If
authentication, a certificate, a revocation or an access check fails, the connection ends; `pb` does not
try a different route. `pb ssh` is allowed only because its byte stream uses this transport and ends at
the machine's own `sshd`.

## Keys and sign-in

- Each machine creates its own identity keys. The private keys stay on that machine: in the operating
  system's credential store, or in files only your user can read on a headless server. They are never
  sent to Paperboat and never appear in logs, diagnostic bundles or audit records.
- Your sign-in and refresh credentials are kept the same way. Sessions are short-lived and each refresh
  replaces the last. No token is ever placed in a URL; the short code `pb login` shows you is only an
  approval handle.
- API tokens expire and can be limited to named scopes (`pb api-token create --expires … --scope …`).
  The HTTP API they call is described in [server/docs/api.md](../../server/docs/api.md) and the
  [OpenAPI document](../../server/docs/openapi.json).

## ENV values

The master password and the optional recovery code unlock your keys on your own devices. Neither is sent
to Paperboat, so Paperboat cannot decrypt your values and cannot reset your password. A machine that
should receive a value has its own key and receives only the selected values, encrypted to that key,
never your personal or team keys. Unlocked keys live in the operating system's credential store; on a
headless Linux server they are held in memory for 15 minutes after `pb env unlock` and never written to
disk.

Removing a machine and rotating keys stops it from reading anything new. It cannot make that machine
forget values it already had: change those secrets at their source too.

## Access and revocation

Every action on a machine is checked against the workspace's policy before it is allowed. For a stolen
computer, revoke it with `pb machine revoke <machine>` or in the dashboard; it loses access at once. Then
run `pb auth logout` on the computer if you still have it. A revoked computer stops retrying and tells you
to run `pb login`. The audit log records who did what to which machine and whether it was allowed, never
terminal or file contents.

## Updates

After the first installation, `pb` updates itself from signed (TUF) release metadata. Before anything is
replaced it checks the signature, the length and SHA-256 of the file, and that the version is not older
than the one installed. By default updates install at 04:00 local time; `pb update settings` changes that
or turns automatic updates off, and `pb update check` and `pb update download` let you look at a release
first. The first installation trusts the installer's HTTPS origin and its pinned hash; see
[install.md](install.md).

## What Paperboat cannot protect

- **A compromised machine.** Traffic is decrypted on your machines. Whoever controls one can read what
  reaches it and use the keys on it. Revoke the machine and rotate what it knew.
- **Content that was already exposed.** Rotating a password or key cannot erase a historical encrypted
  snapshot someone already stole, or values already disclosed.
- **Traffic metadata** (timing, addresses, ciphertext sizes) from anyone who can watch the network.
- **Public links.** They are public by design and not end-to-end; put authentication in your app, or use
  `--private` or `--team`.
- **Your Git repository.** Config sync stores plain files there.

## Running the relay and the edge yourself

You can run the relay and the edge on your own server so that your traffic never touches Paperboat's
infrastructure; see [../../helper/README.md](../../helper/README.md).
