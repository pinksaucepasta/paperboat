# X-43 bodies and refused grant, Linux pair (fix-x43 lane, 2026-11-13)

Machines: two throwaway Linux containers on pbvm2, `fixx43-a` (viewer) and `fixx43-b` (preview owner),
both `pb version` = dev-36da08af7 (the fix commit; main aee62911e plus this change).

| Part | Command (on) | Result |
|---|---|---|
| `--body` without `--enable` | `pb preview inspect somename --body` (b) | `pb: --body and --raw only work together with --enable, for example ...` plus usage; no "could not finish the command" |
| 1 KiB random binary POST | curl POST to the public preview URL, `pb preview inspect <id> --json` (b) | request_body_state=complete, request_body_encoding=base64, decodes byte-identical to the sent file |
| binary response (PNG-like bytes) | same | response_body_state=complete, encoding base64, bytes `89 50 4e 47 00 ff` |
| gzip response (Content-Encoding gzip) | GET (b) | response body decoded: `{"hello":"gzip-world"}`, state complete |
| gzip request body | POST with Content-Encoding gzip (b) | request body decoded: `{"req":"gz"}`, state complete |
| iso-8859-1 request body | POST `caf\xe9`, charset=iso-8859-1 (b) | encoding base64, `Y2Fm6Q==` (bytes as sent) |
| viewer on the other machine | `pb preview inspect <id>` (a) | all four captures listed |
| replay of the binary capture | `pb preview replay <id> <capture>` (a) | replay stored as a new record, origin reached |
| `pb exec` both ways | a->b, b->a | EXEC_A_TO_B_OK, EXEC_B_TO_A_OK |
| `pb connect` | a -> b | CONNECT_OK_42 |

Refused inspector grant (503 -> 403): covered by tests only, not reproduced on a device (a bogus grant needs a
server-resolved access record that the CLI cannot be made to send by hand):
`TestInspectorFailureTypesOnlyProvenRefusals` (localdaemon), `TestInspectorRefusedGrantIs403AndTransportFailureIs503` (localapi).
