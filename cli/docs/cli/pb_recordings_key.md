## pb recordings key

Manage a team's recording key

### Synopsis

Manage the team recording key that terminal recordings are sealed to. A team member who holds a copy of the current key can replace it with `pb recordings key replace`: the new key is made and signed on this computer, and machines accept it only through that signature.

With --json, this command group lists its available commands.

```
pb recordings key [flags]
```

### Options

```
  -h, --help   help for key
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

* [pb recordings](pb_recordings.md)	 - List and replay terminal recordings
* [pb recordings key replace](pb_recordings_key_replace.md)	 - Replace the team's recording key with a new one signed by your copy of the current key

