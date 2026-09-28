## pb team device remove

Revoke a team-owned device without personal takeover

### Synopsis

Revoke a team-owned device without personal takeover

Revoke a team-owned device enrollment after exact confirmation. Ownership does not return to a former personal enroller; access is withdrawn from team members.

A personal device can be shared with a team while ownership stays personal, or explicitly transferred to team ownership. Sharing and capability grants are separate decisions. Removing a team-owned device revokes its enrollment; unsharing a personal device withdraws that team's access.

JSON output is supported with --json.

```
pb team device remove <team> <device-id> [flags]
```

### Options

```
      --confirm string    exact device identifier acknowledging the ownership or revocation effect
      --generation uint   expected current team generation
  -h, --help              help for remove
      --json              print canonical JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb team device](pb_team_device.md)	 - Share devices and manage exact team access

