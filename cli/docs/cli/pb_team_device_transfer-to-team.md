## pb team device transfer-to-team

Explicitly transfer your enrollment to team ownership

### Synopsis

Explicitly transfer your enrollment to team ownership

Transfer a personal device enrollment into team ownership after exact confirmation and generation check. This is different from sharing and withdraws unrelated personal team shares.

A personal device can be shared with a team while ownership stays personal, or explicitly transferred to team ownership. Sharing and capability grants are separate decisions. Removing a team-owned device revokes its enrollment; unsharing a personal device withdraws that team's access.

JSON output is supported with --json.

```
pb team device transfer-to-team <team> <device-id> [flags]
```

### Options

```
      --confirm string    exact device identifier acknowledging the ownership or revocation effect
      --generation uint   expected current team generation
  -h, --help              help for transfer-to-team
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

