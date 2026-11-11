## pb team bucket get

Show the team's bucket and its uses

### Synopsis

Show the team's bucket: its endpoint, region, bucket name, folder prefix, access key ID and when Paperboat last wrote, read and deleted a check object in it. The secret access key is stored encrypted and is never shown, by this command or anywhere else.

JSON output is supported with --json.

```
pb team bucket get <team> [flags]
```

### Options

```
  -h, --help   help for get
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

