## pb recordings key status

Show the team's recording key generation, whether you hold a copy and who is waiting for one

### Synopsis

Show the state of the team's recording key without changing it: the current generation (0 when the team has none yet; it is created when the first recording starts in the console), whether you hold a copy of it, and which viewers with an ENV vault still wait for a copy. No key material is printed.

JSON output is supported with --json.

```
pb recordings key status [flags]
```

### Options

```
  -h, --help          help for status
      --json          print JSON
      --team string   team to show; defaults to the active workspace
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb recordings key](pb_recordings_key.md)	 - Manage a team's recording key

