# F8-16 blocked-header refusal, Linux (cycle 2 build dev-e4d2cc0f8)

Machine f816-linux (throwaway container on pbvm2) (fresh, paired to the test account), pb dev-e4d2cc0f8 = CYCLE 2 (server+edges 2026.11.13.9). One tunnel route; a rule naming a header Paperboat owns is refused, the CLI error names the header, and the route record (incl. etag/generation) is byte-identical before and after. UTC 20261113.

```
$ pb version
  ▄▄▝▀▀▀▀▀▘▄▄   Version dev-e4d2cc0f8
<tunnel>	f816-c2-linux	<url>	public	active
tunnel <tunnel> (pb tunnel create f816-c2-linux --port 9317 --duration 30m --wait --json)
['items'] 
route rte_<tunnel>_default
$ pb tunnel route update <tunnel> rte_<tunnel>_default --request-header set:Paperboat-Visitor-Assertion=x
pb: invalid route rules: request header rule 1: Paperboat-Visitor-Assertion belongs to Paperboat, the connection or the browser and cannot be changed by a route rule. Support reference: support_<id>. Run `pb bugreport --upload` to send the details.
exit 1
$ pb tunnel route update <tunnel> rte_<tunnel>_default --response-header add:X-Paperboat-Foo=1
pb: invalid route rules: response header rule 1: X-Paperboat-Foo belongs to Paperboat, the connection or the browser and cannot be changed by a route rule. Support reference: support_<id>. Run `pb bugreport --upload` to send the details.
exit 1
ROUTE UNCHANGED (route record, incl. etag/generation, identical before and after)
$ pb tunnel delete <tunnel>
Nothing has changed. To confirm within 5 minutes, run:
pb tunnel delete '<tunnel>' --confirm PVVMJP
```
