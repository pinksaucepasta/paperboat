## pb team device share

Share your personal device with a team; grants are separate

### Synopsis

Share your personal device with a team; grants are separate

Share a personal device with one team while keeping personal ownership. Members need explicit capability grants before they can use it.

A personal device can be shared with a team while ownership stays personal, or explicitly transferred to team ownership. Sharing and capability grants are separate decisions. Removing a team-owned device revokes its enrollment; unsharing a personal device withdraws that team's access.

JSON output is supported with --json.

```
pb team device share <team> <device-id> [flags]
```

### Options

```
      --generation uint   expected current team generation
  -h, --help              help for share
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

