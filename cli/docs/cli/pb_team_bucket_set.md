## pb team bucket set

Connect or change the team's bucket

### Synopsis

Save the bucket after Paperboat writes, reads back and deletes a small check object in it; a failing step is named and nothing is saved. Give the secret access key on standard input (--secret-access-key-stdin), never as an argument; leave it out to keep the stored key while changing another field.

JSON output is supported with --json.

```
pb team bucket set <team> [flags]
```

### Options

```
      --access-key-id string      access key ID
      --bucket string             bucket name
      --endpoint string           https address of the S3 service, without a path
  -h, --help                      help for set
      --json                      print JSON
      --path-style                address the bucket in the URL path (MinIO and some other services)
      --prefix string             optional folder inside the bucket
      --region string             region of the bucket
      --secret-access-key-stdin   read the secret access key from standard input
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

