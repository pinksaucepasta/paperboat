# X-43 part 2 by hand, UTC 20261112 21:55-22:15 (IST 03:25-03:45 13 Nov)

## Tunnel capture and replay on the shared fleet (lanex2, dev-3ef4b7b39 / dev-64973afc6): PASS for every owner and viewer
For owner L, M and W: a durable tunnel (`pb tunnel create <name> --port N --wait`) to a small origin, capture enabled with `pb tunnel inspect <name> --enable --body --raw`, a CRLF POST from oracle, then `pb tunnel inspect` and `pb tunnel replay` run on the other two machines:
```
owner L viewer M, owner L viewer W, owner M viewer L, owner M viewer W, owner W viewer L, owner W viewer M:
  capture shows the exact CRLF body and 8+ request headers; replay rc 0; the same URL shows a second record afterwards
```
(own pair, earlier: owner Linux container, viewer Windows runner: same result.) Tunnels deleted and origins stopped by PID afterwards.
## Access refusal across accounts: PASS
Owner account B had a preview with capture on; the other account C (a different OS user on the same Linux host) runs `pb preview inspect <preview>` and `pb preview replay <preview> <id>`: both `inspector_not_authorized` (authorization_or_entitlement); the owner's inspect works.
## Still open for X-43
Binary, gzip and non-UTF-8 request bodies are not captured (state unsupported, failures.md), the console inspector page in a browser on another OS, timezone of the viewer, capture storage bound and removal with the preview, `localhost` resolution for the replay origin.
