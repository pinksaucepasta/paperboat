## pb env vault list

List the workspace's named ENV vaults

### Synopsis

List the named vaults of the active workspace with their key epoch, value revision and
the recipes that read them. Values are never printed; pb env run shows them to a
command on a machine that may read them.

JSON output is supported with --json.

```
pb env vault list [flags]
```

### Options

```
  -h, --help   help for list
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

* [pb env vault](pb_env_vault.md)	 - Create, list and delete named ENV vaults that recipes read

