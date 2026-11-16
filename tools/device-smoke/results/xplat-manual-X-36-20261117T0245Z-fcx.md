# X-36 newest vs previous build, one pair (Linux containers on pbvm2), by hand after the 6232eebec regression fix

Throwaway containers `fcx-old` (dev-3c91531a7, built before 6232eebec) and `fcx-new` (dev-2dcbbf43b = 924f11b49 + the skew fix; kept before the 10b5adaea build hold), same account, prod server. Both revoked by exact id after the run.

- NEW -> OLD, default workspace: `pb exec fcx-old -- echo` worked (dev-9b6ab4461 and dev-2dcbbf43b).
- OLD -> NEW: `pb exec fcx-new -- echo` worked.
- NEW -> OLD with `pb --workspace <team> exec fcx-old` (both machines shared into the team with exec grants): before the fix the failure read "exec_start_uncertain / could not complete the request"; with dev-2dcbbf43b:
  `pb: fcx-old runs pb dev-3c91531a7 and this pb is dev-2dcbbf43b; a build older than the other can refuse its connections. Update the older one (pb update on that machine), then retry.`
- Control: NEW -> NEW with `--workspace <team>` worked.
- Release versions order, so the message says "older than this pb"; dev builds do not order, so it names both builds.
Unit: TestRefusedStreamNamesAnOlderMachine (cmd/pb, -race, pbvm2).
