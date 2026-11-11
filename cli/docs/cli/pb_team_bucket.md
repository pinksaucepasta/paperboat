## pb team bucket

The team's own S3-compatible bucket (owners and admins)

### Synopsis

A team connects its own S3-compatible bucket and chooses what it is used for: recordings (machines copy closed terminal recordings there, so they replay when the machine is offline), files (files for machines that are offline wait there), or both. Paperboat stores the access key encrypted and only signs short-lived links; everything in the bucket is encrypted before it leaves the machine or browser, and the bucket's own security is the team's.

With --json, this command group lists its available commands.

```
pb team bucket [flags]
```

### Options

```
  -h, --help   help for bucket
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --json               print machine-readable JSON
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb team](pb_team.md)	 - Manage teams and explicit resource permissions
* [pb team bucket get](pb_team_bucket_get.md)	 - Show the team's bucket and its uses
* [pb team bucket remove](pb_team_bucket_remove.md)	 - Disconnect the team's bucket
* [pb team bucket set](pb_team_bucket_set.md)	 - Connect or change the team's bucket

