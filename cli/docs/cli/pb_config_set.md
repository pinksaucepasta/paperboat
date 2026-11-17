## pb config set

Set a local configuration value

### Synopsis

Set one local configuration key of this CLI installation. The keys are server (the
Paperboat server URL: absolute http or https, without credentials, query or fragment) and
ssh-target-port (1-65535, the port registered for this machine's managed SSH target). Check the
effective value afterwards with pb config show; pb config unset server removes the setting.

JSON output is supported with --json.

```
pb config set <key> <value> [flags]
```

### Examples

```
  pb config set server https://api.example.test
  pb config set ssh-target-port 2222
  pb config set server https://api.example.test --json
```

### Options

```
  -h, --help   help for set
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

* [pb config](pb_config.md)	 - Inspect the local CLI config

