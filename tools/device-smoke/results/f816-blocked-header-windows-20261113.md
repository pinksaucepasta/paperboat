# F8-16 blocked-header refusal, Windows (cycle 2 build dev-e4d2cc0f8)

Machine winpool-f816-7 (winpool-7, GitHub Windows Server runner, paired to the test account), pb dev-e4d2cc0f8 = CYCLE 2 (server+edges 2026.11.13.9). One tunnel route; rules naming headers Paperboat owns are refused (PowerShell wraps the stderr line, the message is intact), the CLI error names the header, and the route record (etag, generation) is identical before and after. UTC 20261113.

```
$ pb version
  ▄▄▝▀▀▀▀▀▘▄▄   Version dev-e4d2cc0f8
$ pb tunnel create f816-c2-win --port 9317 --duration 30m --wait --timeout 90s
<url> -> http://127.0.0.1:9317
Status: ready
tunnel <tunnel>
$ pb tunnel route update <tunnel> rte_<tunnel>_default --request-header set:Paperboat-Visitor-Assertion=x
exit 1pb.exe : pb: invalid route rules: request header rule 1: Paperboat-Visitor-Assertion belongs to Paperboat, the 
connection or the browser and cannot be changed by a route rule. Support reference: 
support_<id>. Run `pb bugreport --upload` to send the details.
At line:1 char:1
+ & $pb tunnel route update $T $R --request-header set:Paperboat-Visito ...
+ ~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~
    + CategoryInfo          : NotSpecified: (pb: invalid rou...nd the details.:String) [], RemoteException
    + FullyQualifiedErrorId : NativeCommandError
 

$ pb tunnel route update <tunnel> rte_<tunnel>_default --response-header add:X-Paperboat-Foo=1
pb.exe : pb: invalid route rules: response header rule 1: X-Paperboat-Foo belongs to Paperboat, the connection or the 
browser and cannot be changed by a route rule. Support reference: support_<id>. Run 
`pb bugreport --upload` to send the details.
At line:1 char:1
+ & $pb tunnel route update $T $R --response-header add:X-Paperboat-Foo ...
+ ~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~~
    + CategoryInfo          : NotSpecified: (pb: invalid rou...nd the details.:String) [], RemoteException
    + FullyQualifiedErrorId : NativeCommandError
 
exit 1
ROUTE UNCHANGED (route record, incl. etag/generation, identical before and after)
$ pb tunnel delete <tunnel>
Deleted tunnel <tunnel>; status: draining (running, operation operation_c042b60a-ef0d-429a-bada-24b827084beb)
```
