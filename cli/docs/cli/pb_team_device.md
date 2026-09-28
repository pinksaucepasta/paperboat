## pb team device

Share devices and manage exact team access

### Synopsis

Share a personal enrollment or explicitly transfer it to a team. Each teammate uses their own PB account and starts separate terminal sessions. All remote work runs as the enrolled OS user, so it shares that user's OS file and process permissions. Tunnel management covers existing private/team tunnels; create tunnels locally on the target device. Device grants do not grant ENV administration, public publication, resharing, or attachment to another person's terminal.

With --json, this command group lists its available commands.

### Options

```
  -h, --help   help for device
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --json               print machine-readable JSON
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb team](pb_team.md)	 - Manage teams and explicit resource permissions
* [pb team device grant](pb_team_device_grant.md)	 - Set all-member or selected-member device capabilities
* [pb team device remove](pb_team_device_remove.md)	 - Revoke a team-owned device without personal takeover
* [pb team device share](pb_team_device_share.md)	 - Share your personal device with a team; grants are separate
* [pb team device transfer-to-team](pb_team_device_transfer-to-team.md)	 - Explicitly transfer your enrollment to team ownership
* [pb team device unshare](pb_team_device_unshare.md)	 - Withdraw the team's grants while retaining personal ownership

