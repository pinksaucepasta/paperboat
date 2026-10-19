## pb pair

Enroll this machine with a one-shot token

### Synopsis

Enroll this machine with a one-shot token

Enroll this machine with a one-shot token from the dashboard Add machine command or pb machine add, optionally reading it from a private file. pb login is the way to add a computer with browser approval. Name, shell, and state-root options target the local installation; enrollment binds a new machine identity to the account.

JSON output is supported with --json.

```
pb pair [flags]
```

### Options

```
      --enrollment-token string        single-use pairing token
      --enrollment-token-file string   absolute protected file containing a single-use pairing token
  -h, --help                           help for pair
      --json                           print JSON
      --name string                    machine name
      --shell string                   absolute login shell
      --state-root string              runtime state directory
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb](pb.md)	 - Open Paperboat or connect to an environment terminal

