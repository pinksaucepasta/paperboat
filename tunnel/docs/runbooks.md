# Runbooks

## Native ingress regression

Confirm the affected protocol, certificate selection, route generation, connector
session, and upstream target independently. HTTP/3 failure must not be described as
successful native ingress merely because HTTP/2 works.

## Connector or private-access failure

Verify current enrollment, node/process generation, admission expiry, account and route
bindings, then retry with fresh authority. Do not reuse stale credentials or broaden a
grant.

## Shutdown or drain failure

Stop new admission, allow bounded admitted work to finish, cancel remaining operations,
and confirm listeners and connector sessions close before restart.
