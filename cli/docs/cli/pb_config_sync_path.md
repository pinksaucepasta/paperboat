## pb config sync path

Show the authoritative machine configuration path

### Synopsis

Show the authoritative machine configuration path

Print the authoritative local machine configuration path so you can edit or inspect the source file. This command only resolves the location; it does not create a file or change the machine's synchronization assignment.

Configuration commands distinguish local CLI settings from synchronized repository content. Inspect current state before changing it; synchronization reports conflicts instead of silently overwriting one side. The --json option provides structured output for scripts where supported.

JSON output is supported with --json.

```
pb config sync path [flags]
```

### Options

```
  -h, --help   help for path
      --json   print the authoritative path as JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb config sync](pb_config_sync.md)	 - Validate and apply this machine's configuration file

