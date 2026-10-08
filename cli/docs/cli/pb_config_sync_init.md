## pb config sync init

Create an empty machine configuration without overwriting existing files

### Synopsis

Create an empty machine configuration without overwriting existing files

Create a protected empty machine configuration at its authoritative local path. An existing file is preserved and reported as an error. Edit the created file to define repository choices and explicit synchronization path rules.

Configuration commands distinguish local CLI settings from synchronized repository content. Inspect current state before changing it; synchronization reports conflicts instead of silently overwriting one side. The --json option provides structured output for scripts where supported.

JSON output is supported with --json.

```
pb config sync init [flags]
```

### Options

```
  -h, --help   help for init
      --json   print the initialized path as JSON
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

