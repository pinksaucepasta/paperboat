## pb team machine

Share machines and manage exact team access

### Synopsis

Share a personal enrollment or explicitly transfer it to a team. Each teammate uses their own PB account and starts separate terminal sessions. All remote work runs as the enrolled OS user, so it shares that user's OS file and process permissions. Tunnel management covers existing private/team tunnels; create tunnels locally on the target machine. Machine grants do not grant ENV administration, public publication, resharing, or attachment to another person's terminal.

With --json, this command group lists its available commands.

### Options

```
  -h, --help   help for machine
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
* [pb team machine grant](pb_team_machine_grant.md)	 - Set all-member or selected-member machine capabilities
* [pb team machine remove](pb_team_machine_remove.md)	 - Revoke a team-owned machine without personal takeover
* [pb team machine share](pb_team_machine_share.md)	 - Share your personal machine with a team; grants are separate
* [pb team machine transfer-to-team](pb_team_machine_transfer-to-team.md)	 - Explicitly transfer your enrollment to team ownership
* [pb team machine unshare](pb_team_machine_unshare.md)	 - Withdraw the team's grants while retaining personal ownership

