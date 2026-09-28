## pb team device unshare

Withdraw the team's grants while retaining personal ownership

### Synopsis

Withdraw the team's grants while retaining personal ownership

Withdraw one team's access to a personally owned device while retaining that enrollment. Other independent personal ownership remains intact.

A personal device can be shared with a team while ownership stays personal, or explicitly transferred to team ownership. Sharing and capability grants are separate decisions. Removing a team-owned device revokes its enrollment; unsharing a personal device withdraws that team's access.

JSON output is supported with --json.

```
pb team device unshare <team> <device-id> [flags]
```

### Options

```
      --generation uint   expected current team generation
  -h, --help              help for unshare
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

