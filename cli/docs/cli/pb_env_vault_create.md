## pb env vault create

Create a named ENV vault

### Synopsis

Create a named vault in the active workspace. Its key is made on this computer and
wrapped with your ENV keys (and the team's, in a team workspace), so unlock them first
with pb env unlock. Names use lowercase letters, digits and -, up to 63 characters.

JSON output is supported with --json.

```
pb env vault create <vault> [flags]
```

### Options

```
  -h, --help   help for create
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

