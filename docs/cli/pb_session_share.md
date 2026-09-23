## pb session share

Grant a team or teammate viewer or interactive access

### Synopsis

Sharing includes up to 64 KiB of recent output, further bounded by attachment capacity, then live output. Recent output may reveal secrets regardless of ENV permissions. Interactive typing may interleave in host receive order and uses the shell's OS permissions.

JSON output is supported with --json.

```
pb session share <session-id> [flags]
```

### Options

```
      --all             share with all team members
  -h, --help            help for share
      --json            print canonical JSON
      --member string   selected teammate account ID
      --role string     viewer or interactive (default "viewer")
      --team string     team ID
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb session](pb_session.md)	 - Manage environment terminal sessions

