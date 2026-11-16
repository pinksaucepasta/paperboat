## pb env vault delete

Delete a named ENV vault and its values

### Synopsis

Delete a named vault and its values. A vault a recipe still reads cannot be deleted:
remove it from the recipe's [env] vaults and run pb recipe update first.

JSON output is supported with --json.

```
pb env vault delete <vault> [flags]
```

### Options

```
  -h, --help   help for delete
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

