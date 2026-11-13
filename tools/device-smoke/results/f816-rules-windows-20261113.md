# F8-16 Windows by hand, UTC 20261113

Machine winpool-f816-1 (hosted Windows runner winpool-14), pb dev-aee62911e = CYCLE 1 build, same method as the Linux transcript. 10 of 11 steps PASS; the blocked-header refusal prints only the generic message (fixed bcd7c24f2, verify next cycle; passed on my own dev-2d693ecd5 build, second transcript, not a cycle build). The route-update 502 window (failures.md) shows in the timelines.

# Transcript 1: cycle build

# F8-16 windows, 2026-11-13 01:17 UTC
$ pb version
  ▄▄▝▀▀▀▀▀▘▄▄   Version dev-aee62911e

### setup: tunnel with an echo origin, served by both edges
$ pb tunnel create f816-windows-32654 --port 9317 --duration 90m --wait --timeout 90s --json
$ pb tunnel route list tunnel_68eececb-3244-4ee0-b0f1-9775a99cab62 --json
tunnel tunnel_68eececb-3244-4ee0-b0f1-9775a99cab62 url https://<tunnel>.example.test route rte_tunnel_68eececb-3244-4ee0-b0f1-9775a99cab62_default
RESULT PASS (23s) both edges answer ['<edge-ip>', '<edge-ip>'] (first 200 after 1.0s)
status codes seen while polling so far: {200: 2}

### baseline: no rules
app saw X-F816-Set=['client']; response X-App-Remove=['remove-me'] X-App-Set=['app-original']
RESULT PASS (0s) no rules, nothing changed
status codes seen while polling so far: {200: 4}

### request headers add / set / remove reach the app
$ pb tunnel route update tunnel_68eececb-3244-4ee0-b0f1-9775a99cab62 rte_tunnel_68eececb-3244-4ee0-b0f1-9775a99cab62_default --request-header add:X-F816-Add=one --request-header set:X-F816-Set=two --request-header remove:X-F816-Remove
Updated route rte_tunnel_68eececb-3244-4ee0-b0f1-9775a99cab62_default; status: connecting (running, operation operation_812ee0f8-df2a-4e95-aa15-1411eec4212a)
  status timeline per edge (s:code): <edge-ip> 1:200 2:502 4:200 6:200; <edge-ip> 1:200 2:502 4:502 6:200
app saw: X-F816-Add=['client', 'one'] (client value + added) X-F816-Set=['two'] (replaced) X-F816-Remove=absent X-F816-Keep=['client']
RESULT PASS (10s) applied on every edge in 6.2s
status codes seen while polling so far: {200: 9, 502: 3}

### response headers add / set / remove reach the visitor
$ pb tunnel route update tunnel_68eececb-3244-4ee0-b0f1-9775a99cab62 rte_tunnel_68eececb-3244-4ee0-b0f1-9775a99cab62_default --response-header add:X-F816-Resp=added --response-header set:X-App-Set=rule-set --response-header remove:X-App-Remove
Updated route rte_tunnel_68eececb-3244-4ee0-b0f1-9775a99cab62_default; status: connecting (running, operation operation_50fa1908-b702-4732-a52d-e40af7a9e48a)
  status timeline per edge (s:code): <edge-ip> 1:200 2:200 4:200 5:200 6:502 8:502 9:502 10:502 12:502 13:200 15:200 16:200 18:200 19:200 20:200 25:200; <edge-ip> 1:200 2:502 4:502 5:502 6:502 8:502 9:502 10:502 12:502 13:502 15:502 16:502 18:502 19:502 20:502 25:200
response: X-F816-Resp=['added'] X-App-Set=['rule-set'] (app said app-original) X-App-Remove=absent (app sent it) X-App-Keep=['keep']
RESULT PASS (29s) applied on every edge in 25.0s
status codes seen while polling so far: {200: 22, 502: 22}

### path prefix rewritten before the app
$ pb tunnel route update tunnel_68eececb-3244-4ee0-b0f1-9775a99cab62 rte_tunnel_68eececb-3244-4ee0-b0f1-9775a99cab62_default --rewrite-path '/api -> /v1'
Updated route rte_tunnel_68eececb-3244-4ee0-b0f1-9775a99cab62_default; status: connecting (running, operation operation_1138e347-5e25-4c59-b6a3-e85e4f96f05d)
  status timeline per edge (s:code): <edge-ip> 1:200 2:502 4:200 5:200 7:200 8:200 9:200 12:200; <edge-ip> 1:200 2:502 4:502 5:502 7:502 8:502 9:502 12:200
GET /api -> app saw /v1 (every edge)
GET /api/users?x=1 -> app saw /v1/users?x=1 (every edge)
GET /apix -> app saw /apix (every edge)
GET /other/api -> app saw /other/api (every edge)
RESULT PASS (18s) 
status codes seen while polling so far: {200: 39, 502: 29}

### /old answers 301/302/307/308 to a path and an https URL without reaching the app
$ pb tunnel route update tunnel_68eececb-3244-4ee0-b0f1-9775a99cab62 rte_tunnel_68eececb-3244-4ee0-b0f1-9775a99cab62_default --redirect '/old -> 301:/new' --redirect '/moved -> 302:https://example.test/landing' --redirect '/tmp -> 307:/temp' --redirect '/perm -> 308:/permanent'
Updated route rte_tunnel_68eececb-3244-4ee0-b0f1-9775a99cab62_default; status: connecting (running, operation operation_e1ce0d74-d46b-4c4c-b920-1c4ddb89da5b)
  status timeline per edge (s:code): <edge-ip> 1:200 2:200 4:301 5:301 6:301 7:301 9:301 10:301 11:301; <edge-ip> 1:200 2:200 4:200 5:502 6:502 7:502 9:502 10:502 11:301
GET /old -> 301 Location: /new (every edge)
GET /old/page?q=1 -> 301 Location: /new/page?q=1 (every edge)
GET /moved -> 302 Location: https://example.test/landing (every edge)
GET /tmp -> 307 Location: /temp (every edge)
GET /perm -> 308 Location: /permanent (every edge)
GET /oldish -> 200 (a prefix matches only at a segment boundary)
app request log has 45 entries before and after 30 more redirected requests (5 paths x 3 rounds x 2 edges): the redirected requests never reached the app
RESULT PASS (20s) 
status codes seen while polling so far: {200: 44, 502: 34, 301: 12, 302: 2, 307: 2, 308: 2}

### Paperboat's own headers cannot be overridden
$ pb tunnel route list tunnel_68eececb-3244-4ee0-b0f1-9775a99cab62 --json
$ pb tunnel route update tunnel_68eececb-3244-4ee0-b0f1-9775a99cab62 rte_tunnel_68eececb-3244-4ee0-b0f1-9775a99cab62_default --request-header set:X-Forwarded-For=evil
RESULT FAIL: ('set:X-Forwarded-For=evil', 1, 'pb: Paperboat could not finish the command. Check the current state before retrying; run `pb doctor` if this continues. Support reference: support_c4a1896b-ad9e-4997-972e-0eb03b9db419. Run `pb bugrepo')

### route add with the rule flags
$ pb tunnel route add tunnel_68eececb-3244-4ee0-b0f1-9775a99cab62 --name extra --to http://127.0.0.1:9317 --path '/extra*' --request-header set:X-F816-Route=extra --response-header add:X-F816-Extra=yes --rewrite-path '/extra -> /inner' --redirect '/extra/go -> 308:/extra/there' --priority 10
Created route route_8ecc7120-12d1-48af-94fd-2b3af3530e9c; status: connecting (running, operation operation_d488dbea-3ac0-4bf5-913d-df55c467cf56)
  status timeline per edge (s:code): <edge-ip> 1:200 3:200 5:200 6:200 8:200 10:200 12:200 13:200; <edge-ip> 1:200 3:200 5:200 6:200 8:200 10:200 12:200 13:200
route "extra" (added with the rule flags) applies on /extra* only: /extra/x -> app saw /inner/x with X-F816-Route; /echo carries none of it
RESULT PASS (18s) applied on every edge in 13.3s
status codes seen while polling so far: {200: 60, 502: 34, 301: 12, 302: 2, 307: 2, 308: 4}

### access checks run before rules (private tunnel)
$ pb tunnel create f816-windows-32654-p --port 9317 --private --duration 60m --wait --timeout 90s --json
$ pb tunnel route list tunnel_29785f74-3725-44e2-8979-88643cecf72e --json
$ pb tunnel route update tunnel_29785f74-3725-44e2-8979-88643cecf72e rte_tunnel_29785f74-3725-44e2-8979-88643cecf72e_default --redirect '/old -> 301:/new' --request-header set:X-F816-Priv=1 --rewrite-path '/api -> /v1'
private tunnel, no credentials: GET / -> 401 (Location None); GET /old -> 401 (Location None)
forged assertion header + cookie: GET /old -> 401; GET /api/x (a rewrite path) -> 401 (same refusal)
the origin logged 62 requests before and 62 after 4 requests to the private tunnel: none of them reached the app
RESULT PASS (39s) 
status codes seen while polling so far: {200: 60, 502: 34, 301: 12, 302: 2, 307: 2, 308: 4, 401: 2}

### previews are excluded
$ pb preview 9317 --background --ttl 30m --json
preview preview_ab407bd1-84ef-435b-90d0-f77744342dd0 https://<tunnel>.example.test
preview of the same port: /api/x reaches the app as /api/x, X-F816-Set stays client, no X-F816-Add, response X-App-Remove kept, no X-F816-Resp: the route rules do not apply
preview GET /old -> 200 from the app (no redirect rule)
$ pb preview 9317 --background --request-header add:X-A=b
pb preview has no rule flags: pb: The command arguments are invalid. Run `pb COMMAND --help` and retry.
RESULT PASS (17s) 
status codes seen while polling so far: {200: 60, 502: 34, 301: 12, 302: 2, 307: 2, 308: 4, 401: 2}

### --clear-* flags restore the baseline
$ pb tunnel route update tunnel_68eececb-3244-4ee0-b0f1-9775a99cab62 rte_tunnel_68eececb-3244-4ee0-b0f1-9775a99cab62_default --clear-request-headers --clear-response-headers --clear-rewrite-path --clear-redirects
Updated route rte_tunnel_68eececb-3244-4ee0-b0f1-9775a99cab62_default; status: connecting (running, operation operation_17d52beb-5744-40c2-838a-ff51ebbbf912)
  status timeline per edge (s:code): <edge-ip> 1:200 2:200 4:502 5:502 6:200 8:200 9:200 11:200 13:200; <edge-ip> 1:200 2:200 4:502 5:502 6:502 8:502 9:502 11:502 13:200
RESULT PASS (19s) all four lists cleared, behaves as baseline within 13.3s
status codes seen while polling so far: {200: 70, 502: 42, 301: 12, 302: 2, 307: 2, 308: 4, 401: 2}

### cleanup
$ pb tunnel delete tunnel_68eececb-3244-4ee0-b0f1-9775a99cab62 --json
$ pb tunnel delete tunnel_68eececb-3244-4ee0-b0f1-9775a99cab62 --confirm 6E7RZ7 --json
$ pb tunnel delete tunnel_29785f74-3725-44e2-8979-88643cecf72e --json
$ pb tunnel delete tunnel_29785f74-3725-44e2-8979-88643cecf72e --confirm FFMQYU --json
$ pb preview stop preview_ab407bd1-84ef-435b-90d0-f77744342dd0

## summary
PASS  setup: tunnel with an echo origin, served by both edges
PASS  baseline: no rules
PASS  request headers add / set / remove reach the app
PASS  response headers add / set / remove reach the visitor
PASS  path prefix rewritten before the app
PASS  /old answers 301/302/307/308 to a path and an https URL without reaching the app
FAIL  Paperboat's own headers cannot be overridden
PASS  route add with the rule flags
PASS  access checks run before rules (private tunnel)
PASS  previews are excluded
PASS  --clear-* flags restore the baseline



---

# Transcript 2: supplement on my own build dev-2d693ecd5 (the fix, NOT a cycle build), all 11 steps PASS (access step re-run separately at the end)

# F8-16 windows, 2026-11-13 00:38 UTC
$ pb version
  ▄▄▝▀▀▀▀▀▘▄▄   Version dev-2d693ecd5

### setup: tunnel with an echo origin, served by both edges
$ pb tunnel create f816-windows-30289 --port 9317 --duration 90m --wait --timeout 90s --json
$ pb tunnel route list tunnel_f4d75f46-c687-42ba-914a-b7291d140b84 --json
tunnel tunnel_f4d75f46-c687-42ba-914a-b7291d140b84 url https://<tunnel>.example.test route rte_tunnel_f4d75f46-c687-42ba-914a-b7291d140b84_default
RESULT PASS (30s) both edges answer ['<edge-ip>', '<edge-ip>'] (first 200 after 0.7s)
status codes seen while polling so far: {200: 2}

### baseline: no rules
app saw X-F816-Set=['client']; response X-App-Remove=['remove-me'] X-App-Set=['app-original']
RESULT PASS (0s) no rules, nothing changed
status codes seen while polling so far: {200: 4}

### request headers add / set / remove reach the app
$ pb tunnel route update tunnel_f4d75f46-c687-42ba-914a-b7291d140b84 rte_tunnel_f4d75f46-c687-42ba-914a-b7291d140b84_default --request-header add:X-F816-Add=one --request-header set:X-F816-Set=two --request-header remove:X-F816-Remove
Updated route rte_tunnel_f4d75f46-c687-42ba-914a-b7291d140b84_default; status: connecting (running, operation operation_43841c04-7c03-4a5b-8268-2fc703d59af2)
  status timeline per edge (s:code): <edge-ip> 1:200 3:200 4:200 5:502 7:200 8:200 10:200; <edge-ip> 1:200 3:200 4:502 5:502 7:502 8:502 10:200
app saw: X-F816-Add=['client', 'one'] (client value + added) X-F816-Set=['two'] (replaced) X-F816-Remove=absent X-F816-Keep=['client']
RESULT PASS (14s) applied on every edge in 10.3s
status codes seen while polling so far: {200: 13, 502: 5}

### response headers add / set / remove reach the visitor
$ pb tunnel route update tunnel_f4d75f46-c687-42ba-914a-b7291d140b84 rte_tunnel_f4d75f46-c687-42ba-914a-b7291d140b84_default --response-header add:X-F816-Resp=added --response-header set:X-App-Set=rule-set --response-header remove:X-App-Remove
Updated route rte_tunnel_f4d75f46-c687-42ba-914a-b7291d140b84_default; status: connecting (running, operation operation_3336f109-be92-4a8f-9bf8-23d6615fcf1c)
  status timeline per edge (s:code): <edge-ip> 0:200 2:200 3:502 4:502 6:502 7:502 8:502 10:502 11:200 13:200 14:200; <edge-ip> 0:502 2:502 3:502 4:502 6:502 7:502 8:502 10:502 11:502 13:502 14:200
response: X-F816-Resp=['added'] X-App-Set=['rule-set'] (app said app-original) X-App-Remove=absent (app sent it) X-App-Keep=['keep']
RESULT PASS (19s) applied on every edge in 14.4s
status codes seen while polling so far: {200: 19, 502: 21}

### path prefix rewritten before the app
$ pb tunnel route update tunnel_f4d75f46-c687-42ba-914a-b7291d140b84 rte_tunnel_f4d75f46-c687-42ba-914a-b7291d140b84_default --rewrite-path '/api -> /v1'
Updated route rte_tunnel_f4d75f46-c687-42ba-914a-b7291d140b84_default; status: connecting (running, operation operation_1b2939e4-4cd2-49dc-bb0d-3be2da48b339)
  status timeline per edge (s:code): <edge-ip> 1:200 2:200 3:502 4:502 6:502 7:502 8:502 9:502 10:502 11:502 13:200 14:200 16:200; <edge-ip> 1:200 2:502 3:502 4:502 6:502 7:502 8:502 9:502 10:502 11:502 13:502 14:502 16:200
GET /api -> app saw /v1 (every edge)
GET /api/users?x=1 -> app saw /v1/users?x=1 (every edge)
GET /apix -> app saw /apix (every edge)
GET /other/api -> app saw /other/api (every edge)
RESULT PASS (22s) 
status codes seen while polling so far: {200: 34, 502: 40}

### /old answers 301/302/307/308 to a path and an https URL without reaching the app
$ pb tunnel route update tunnel_f4d75f46-c687-42ba-914a-b7291d140b84 rte_tunnel_f4d75f46-c687-42ba-914a-b7291d140b84_default --redirect '/old -> 301:/new' --redirect '/moved -> 302:https://example.test/landing' --redirect '/tmp -> 307:/temp' --redirect '/perm -> 308:/permanent'
Updated route rte_tunnel_f4d75f46-c687-42ba-914a-b7291d140b84_default; status: connecting (running, operation operation_ec2ad245-7ab5-4e5d-b6c7-61e8ba625706)
  status timeline per edge (s:code): <edge-ip> 0:502 2:502 3:502 4:502 5:502 6:502 7:502 9:301 10:301 11:301 12:301; <edge-ip> 0:200 2:200 3:502 4:502 5:502 6:502 7:502 9:502 10:502 11:502 12:301
GET /old -> 301 Location: /new (every edge)
GET /old/page?q=1 -> 301 Location: /new/page?q=1 (every edge)
GET /moved -> 302 Location: https://example.test/landing (every edge)
GET /tmp -> 307 Location: /temp (every edge)
GET /perm -> 308 Location: /permanent (every edge)
GET /oldish -> 200 (a prefix matches only at a segment boundary)
app request log has 37 entries before and after 30 more redirected requests (5 paths x 3 rounds x 2 edges): the redirected requests never reached the app
RESULT PASS (22s) 
status codes seen while polling so far: {200: 36, 502: 55, 301: 9, 302: 2, 307: 2, 308: 2}

### Paperboat's own headers cannot be overridden
$ pb tunnel route list tunnel_f4d75f46-c687-42ba-914a-b7291d140b84 --json
$ pb tunnel route update tunnel_f4d75f46-c687-42ba-914a-b7291d140b84 rte_tunnel_f4d75f46-c687-42ba-914a-b7291d140b84_default --request-header set:X-Forwarded-For=evil
  refused (exit 1): pb: invalid route rules: request header rule 1: X-Forwarded-For belongs to Paperboat, the connection or the browser and cannot be changed by a route rule. Support reference: support_dc41fd7d-25e5-4175
$ pb tunnel route update tunnel_f4d75f46-c687-42ba-914a-b7291d140b84 rte_tunnel_f4d75f46-c687-42ba-914a-b7291d140b84_default --request-header add:Forwarded=for=evil
  refused (exit 1): pb: invalid route rules: request header rule 1: Forwarded belongs to Paperboat, the connection or the browser and cannot be changed by a route rule. Support reference: support_6a83bee0-80c0-4822-97fd-
$ pb tunnel route update tunnel_f4d75f46-c687-42ba-914a-b7291d140b84 rte_tunnel_f4d75f46-c687-42ba-914a-b7291d140b84_default --request-header set:Paperboat-Visitor-Assertion=x
  refused (exit 1): pb: invalid route rules: request header rule 1: Paperboat-Visitor-Assertion belongs to Paperboat, the connection or the browser and cannot be changed by a route rule. Support reference: support_4ac2b2
$ pb tunnel route update tunnel_f4d75f46-c687-42ba-914a-b7291d140b84 rte_tunnel_f4d75f46-c687-42ba-914a-b7291d140b84_default --request-header set:X-Paperboat-Foo=1
  refused (exit 1): pb: invalid route rules: request header rule 1: X-Paperboat-Foo belongs to Paperboat, the connection or the browser and cannot be changed by a route rule. Support reference: support_9861e5f7-f56a-4db1
$ pb tunnel route update tunnel_f4d75f46-c687-42ba-914a-b7291d140b84 rte_tunnel_f4d75f46-c687-42ba-914a-b7291d140b84_default --request-header set:Host=evil
  refused (exit 1): pb: invalid route rules: request header rule 1: Host belongs to Paperboat, the connection or the browser and cannot be changed by a route rule. Support reference: support_cd23124d-bccd-494e-9e23-4204b
$ pb tunnel route update tunnel_f4d75f46-c687-42ba-914a-b7291d140b84 rte_tunnel_f4d75f46-c687-42ba-914a-b7291d140b84_default --request-header set:Authorization=x
  refused (exit 1): pb: invalid route rules: request header rule 1: Authorization belongs to Paperboat, the connection or the browser and cannot be changed by a route rule. Support reference: support_2efe3990-576c-435e-a
$ pb tunnel route update tunnel_f4d75f46-c687-42ba-914a-b7291d140b84 rte_tunnel_f4d75f46-c687-42ba-914a-b7291d140b84_default --request-header remove:X-Real-Ip
  refused (exit 1): pb: invalid route rules: request header rule 1: X-Real-Ip belongs to Paperboat, the connection or the browser and cannot be changed by a route rule. Support reference: support_af6929e2-a8ae-46ac-9bec-
$ pb tunnel route update tunnel_f4d75f46-c687-42ba-914a-b7291d140b84 rte_tunnel_f4d75f46-c687-42ba-914a-b7291d140b84_default --response-header set:Strict-Transport-Security=x
  refused (exit 1): pb: invalid route rules: response header rule 1: Strict-Transport-Security belongs to Paperboat, the connection or the browser and cannot be changed by a route rule. Support reference: support_e5f365d
$ pb tunnel route update tunnel_f4d75f46-c687-42ba-914a-b7291d140b84 rte_tunnel_f4d75f46-c687-42ba-914a-b7291d140b84_default --response-header set:Content-Security-Policy=x
  refused (exit 1): pb: invalid route rules: response header rule 1: Content-Security-Policy belongs to Paperboat, the connection or the browser and cannot be changed by a route rule. Support reference: support_4c6cd851-
$ pb tunnel route update tunnel_f4d75f46-c687-42ba-914a-b7291d140b84 rte_tunnel_f4d75f46-c687-42ba-914a-b7291d140b84_default --response-header set:X-Frame-Options=x
  refused (exit 1): pb: invalid route rules: response header rule 1: X-Frame-Options belongs to Paperboat, the connection or the browser and cannot be changed by a route rule. Support reference: support_0f6d7458-8d62-42e
$ pb tunnel route update tunnel_f4d75f46-c687-42ba-914a-b7291d140b84 rte_tunnel_f4d75f46-c687-42ba-914a-b7291d140b84_default --response-header set:Set-Cookie=a=b
  refused (exit 1): pb: invalid route rules: response header rule 1: Set-Cookie belongs to Paperboat, the connection or the browser and cannot be changed by a route rule. Support reference: support_75df11f0-0f0e-438e-b1f
$ pb tunnel route update tunnel_f4d75f46-c687-42ba-914a-b7291d140b84 rte_tunnel_f4d75f46-c687-42ba-914a-b7291d140b84_default --response-header set:Location=https://evil.test
  refused (exit 1): pb: invalid route rules: response header rule 1: Location belongs to Paperboat, the connection or the browser and cannot be changed by a route rule. Support reference: support_7fdb0c83-da7d-4ab0-bec6-
$ pb tunnel route update tunnel_f4d75f46-c687-42ba-914a-b7291d140b84 rte_tunnel_f4d75f46-c687-42ba-914a-b7291d140b84_default --response-header remove:X-Content-Type-Options
  refused (exit 1): pb: invalid route rules: response header rule 1: X-Content-Type-Options belongs to Paperboat, the connection or the browser and cannot be changed by a route rule. Support reference: support_37d304aa-8
$ pb tunnel route list tunnel_f4d75f46-c687-42ba-914a-b7291d140b84 --json
route unchanged after the refusals (rules identical in `route list`)
forged client headers with rules active: app saw X-Forwarded-For=['<edge-ip>'] X-Forwarded-Proto=['https'] X-Request-Id=request_64e33c62; no Paperboat-Visitor-Assertion, no X-Paperboat-Foo
security response headers on the answer: ['referrer-policy', 'x-content-type-options', 'x-frame-options'] (equal to the no-rules baseline; response headers seen: ['content-length', 'content-type', 'date', 'referrer-policy', 'server', 'x-app-keep', 'x-app-set', 'x-content-type-options', 'x-f816-resp', 'x-frame-options'])
RESULT PASS (67s) 
status codes seen while polling so far: {200: 36, 502: 55, 301: 9, 302: 2, 307: 2, 308: 2}

### route add with the rule flags
$ pb tunnel route add tunnel_f4d75f46-c687-42ba-914a-b7291d140b84 --name extra --to http://127.0.0.1:9317 --path '/extra*' --request-header set:X-F816-Route=extra --response-header add:X-F816-Extra=yes --rewrite-path '/extra -> /inner' --redirect '/extra/go -> 308:/extra/there' --priority 10
Created route route_0651eba8-e969-4b64-99cd-c65b97df501a; status: connecting (running, operation operation_d25f0b47-7534-449c-ad14-47cb0a4db01e)
  status timeline per edge (s:code): <edge-ip> 1:200 2:200 4:200 6:200 7:200 9:200 10:200 12:200 14:200 20:200; <edge-ip> 1:200 2:200 4:200 6:200 7:200 9:200 10:200 12:200 14:200 20:200
route "extra" (added with the rule flags) applies on /extra* only: /extra/x -> app saw /inner/x with X-F816-Route; /echo carries none of it
RESULT PASS (24s) applied on every edge in 19.6s
status codes seen while polling so far: {200: 56, 502: 55, 301: 9, 302: 2, 307: 2, 308: 4}

### access checks run before rules (private tunnel)
$ pb tunnel create f816-windows-30289-p --port 9317 --private --duration 60m --wait --timeout 90s --json
$ pb tunnel route list tunnel_1ec40fd2-ef2b-4275-8b4d-0385d992cb73 --json
$ pb tunnel route update tunnel_1ec40fd2-ef2b-4275-8b4d-0385d992cb73 rte_tunnel_1ec40fd2-ef2b-4275-8b4d-0385d992cb73_default --redirect '/old -> 301:/new' --request-header set:X-F816-Priv=1 --rewrite-path '/api -> /v1'
private tunnel, no credentials: GET / -> 404 (Location None); GET /old -> 401 (Location None)
RESULT FAIL: the redirect rule answered before the access check

### previews are excluded
$ pb preview 9317 --background --ttl 30m --json
preview preview_fefba970-4094-49ec-95c7-d8c0e5c78e81 https://<tunnel>.example.test
preview of the same port: /api/x reaches the app as /api/x, X-F816-Set stays client, no X-F816-Add, response X-App-Remove kept, no X-F816-Resp: the route rules do not apply
preview GET /old -> 200 from the app (no redirect rule)
$ pb preview 9317 --background --request-header add:X-A=b
pb preview has no rule flags: pb: The command arguments are invalid. Run `pb COMMAND --help` and retry.
RESULT PASS (17s) 
status codes seen while polling so far: {200: 56, 502: 55, 301: 9, 302: 2, 307: 2, 308: 4}

### --clear-* flags restore the baseline
$ pb tunnel route update tunnel_f4d75f46-c687-42ba-914a-b7291d140b84 rte_tunnel_f4d75f46-c687-42ba-914a-b7291d140b84_default --clear-request-headers --clear-response-headers --clear-rewrite-path --clear-redirects
Updated route rte_tunnel_f4d75f46-c687-42ba-914a-b7291d140b84_default; status: connecting (running, operation operation_23a8aaac-cf01-437e-988f-e5d4d008f988)
  status timeline per edge (s:code): <edge-ip> 1:200 2:200 4:502 5:502 6:502 8:502 9:502 10:502 11:502 12:502 14:200 15:200 17:200; <edge-ip> 1:200 2:200 4:200 5:200 6:502 8:502 9:502 10:502 11:502 12:502 14:502 15:502 17:200
RESULT PASS (21s) all four lists cleared, behaves as baseline within 16.9s
status codes seen while polling so far: {200: 66, 502: 71, 301: 9, 302: 2, 307: 2, 308: 4}

### cleanup
$ pb tunnel delete tunnel_f4d75f46-c687-42ba-914a-b7291d140b84 --json
$ pb tunnel delete tunnel_f4d75f46-c687-42ba-914a-b7291d140b84 --confirm XM34F3 --json
$ pb tunnel delete tunnel_1ec40fd2-ef2b-4275-8b4d-0385d992cb73 --json
$ pb tunnel delete tunnel_1ec40fd2-ef2b-4275-8b4d-0385d992cb73 --confirm GD6K9Q --json
$ pb preview stop preview_fefba970-4094-49ec-95c7-d8c0e5c78e81

## summary
PASS  setup: tunnel with an echo origin, served by both edges
PASS  baseline: no rules
PASS  request headers add / set / remove reach the app
PASS  response headers add / set / remove reach the visitor
PASS  path prefix rewritten before the app
PASS  /old answers 301/302/307/308 to a path and an https URL without reaching the app
PASS  Paperboat's own headers cannot be overridden
PASS  route add with the rule flags
FAIL  access checks run before rules (private tunnel)
PASS  previews are excluded
PASS  --clear-* flags restore the baseline


---

# F8-16 windows, 2026-11-13 00:45 UTC
$ pb version
  ▄▄▝▀▀▀▀▀▘▄▄   Version dev-2d693ecd5

### setup: tunnel with an echo origin, served by both edges
$ pb tunnel create f816-windows-30759 --port 9317 --duration 90m --wait --timeout 90s --json
$ pb tunnel route list tunnel_038446a2-b105-4bf6-bafc-6723e1276dc7 --json
tunnel tunnel_038446a2-b105-4bf6-bafc-6723e1276dc7 url https://<tunnel>.example.test route rte_tunnel_038446a2-b105-4bf6-bafc-6723e1276dc7_default
RESULT PASS (22s) both edges answer ['<edge-ip>', '<edge-ip>'] (first 200 after 0.9s)
status codes seen while polling so far: {200: 2}

### access checks run before rules (private tunnel)
$ pb tunnel create f816-windows-30759-p --port 9317 --private --duration 60m --wait --timeout 90s --json
$ pb tunnel route list tunnel_b1d0c527-d711-4426-beee-7e131619fdaa --json
$ pb tunnel route update tunnel_b1d0c527-d711-4426-beee-7e131619fdaa rte_tunnel_b1d0c527-d711-4426-beee-7e131619fdaa_default --redirect '/old -> 301:/new' --request-header set:X-F816-Priv=1 --rewrite-path '/api -> /v1'
private tunnel, no credentials: GET / -> 401 (Location None); GET /old -> 401 (Location None)
forged assertion header + cookie: GET /old -> 401; GET /api/x (a rewrite path) -> 401 (same refusal)
the origin logged 2 requests before and 2 after 4 requests to the private tunnel: none of them reached the app
RESULT PASS (36s) 
status codes seen while polling so far: {200: 2, 401: 2}

### cleanup
$ pb tunnel delete tunnel_038446a2-b105-4bf6-bafc-6723e1276dc7 --json
$ pb tunnel delete tunnel_038446a2-b105-4bf6-bafc-6723e1276dc7 --confirm DKMVG5 --json
$ pb tunnel delete tunnel_b1d0c527-d711-4426-beee-7e131619fdaa --json
$ pb tunnel delete tunnel_b1d0c527-d711-4426-beee-7e131619fdaa --confirm YHS4GT --json

## summary
PASS  setup: tunnel with an echo origin, served by both edges
PASS  access checks run before rules (private tunnel)

