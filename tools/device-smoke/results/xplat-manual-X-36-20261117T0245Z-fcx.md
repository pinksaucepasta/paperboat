# X-36 newest vs previous build, one pair (Linux containers on pbvm2), by hand after the 6232eebec regression fix

Throwaway containers `fcx-old` (dev-3c91531a7, built before 6232eebec) and `fcx-new` (dev-2dcbbf43b = 924f11b49 + the skew fix; kept before the 10b5adaea build hold), same account, prod server. Both revoked by exact id after the run.

- NEW -> OLD, default workspace: `pb exec fcx-old -- echo` worked (dev-9b6ab4461 and dev-2dcbbf43b).
- OLD -> NEW: `pb exec fcx-new -- echo` worked.
- NEW -> OLD with `pb --workspace <team> exec fcx-old` (both machines shared into the team with exec grants): before the fix the failure read "exec_start_uncertain / could not complete the request"; with dev-2dcbbf43b:
  `pb: fcx-old runs pb dev-3c91531a7 and this pb is dev-2dcbbf43b; a build older than the other can refuse its connections. Update the older one (pb update on that machine), then retry.`
- Control: NEW -> NEW with `--workspace <team>` worked.
- Release versions order, so the message says "older than this pb"; dev builds do not order, so it names both builds.
Unit: TestRefusedStreamNamesAnOlderMachine (cmd/pb, -race, pbvm2).

## pb connect and pb ssh against the old-build container (second run, same day)
Containers recreated from `dev-3c91531a7` (old) and the pre-hold builds `dev-b638b28dd` / `dev-9156ba16b` (new); both machines shared into the team with exec, terminal, managed_ssh grants; revoked by exact id afterwards.
- `pb connect fcx-old` (default and `--workspace <team>`): worked (online terminal streams carry no workspace in the header). A refused terminal can only occur on the offline path, which needs Paperboat unreachable and a warm cache; with the control plane blocked pb refuses before dialing ("Paperboat is unreachable"), so that path is covered by TestVersionSkewRefusalIsNotRetried only.
- `pb ssh fcx-old -- echo` default: worked. `pb --workspace <team> ssh fcx-old -- echo` NEW->OLD: failed before the fix with only OpenSSH's "Connection closed"; with dev-9156ba16b it ends with `pb: fcx-old runs pb dev-3c91531a7 and this pb is dev-9156ba16b; a build older than the other can refuse its connections. Update the older one (pb update on that machine), then retry.` (an earlier "SSH server closed the connection" line from the existing explainer still prints first). Control NEW->NEW with the same workspace: worked.
