## pb team bucket remove

Disconnect the team's bucket

### Synopsis

Disconnect the bucket. What it already holds stays there, and recordings stay on their machines.

JSON output is supported with --json.

```
pb team bucket remove <team> [flags]
```

### Options

```
  -h, --help   help for remove
      --json   print JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb team bucket](pb_team_bucket.md)	 - The team's own S3-compatible bucket (owners and admins)

