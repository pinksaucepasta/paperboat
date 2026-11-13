# F8-16 console "Edit rules…" once, UTC 20261113 (recorded manual step, driven in the signed-in console tab with page scripts; console served from the live tree)

Tunnel f816-console (public, route default, origin 127.0.0.1:9317, made by pb dev-2d693ecd5 on a throwaway Linux container). Steps and what was seen:
1. /tunnels/<id>: "Public hostnames" lists the route; its menu offers Edit…, Edit rules…, Turn off, Remove hostname.
2. "Edit rules…" opens the dialog "Edit rules" with Request headers, Response headers, Path to replace / Path to use instead, Add redirect.
3. Entered request header X-Console-Rule=from-console, response header X-Console-Resp=yes, rewrite /api -> /v1, redirect /old -> /new; Save was enabled; the dialog closed on save.
4. `pb tunnel route list --json` showed rewrite_rules: request_headers [set X-Console-Rule from-console], response_headers [set X-Console-Resp yes], path_rewrite /api -> /v1, redirects [/old 308 /new] (308 is the default code).
5. curl through both edges after ~15 s: GET /api/x reached the app as /v1/x with X-Console-Rule: from-console; GET /old -> 308 Location: /new; X-Console-Resp: yes on responses.
Re-run at each feature completion by repeating these five steps.
