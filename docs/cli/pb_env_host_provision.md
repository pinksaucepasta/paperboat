## pb env host provision

Provision an explicit encrypted ENV selection to a host

### Synopsis

Provision an explicit encrypted ENV selection to a host

JSON output is supported with --json.

```
pb env host provision [flags]
```

### Options

```
      --empty                explicitly provision an empty selection
  -h, --help                 help for provision
      --json                 print JSON
      --machine string       machine name or ID
      --select stringArray   selection reference personal:NAME or team:TEAM:NAME; repeat to select more values
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
```

### SEE ALSO

* [pb env host](pb_env_host.md)	 - Manage encrypted host ENV projections

