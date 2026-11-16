## pb env vault

Create, list and delete named ENV vaults that recipes read

### Synopsis

A named vault is a set of ENV values with its own key, for example canary-env. Members
of the workspace read it with their ENV keys, and machines paired from a recipe whose
[env] vaults lists it read it with the recipe's token, with nobody online. Set values
with pb env set <name> --vault <vault>.

With --json, this command group lists its available commands.

```
pb env vault [flags]
```

### Options

```
  -h, --help   help for vault
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

* [pb env](pb_env.md)	 - Manage automatic global ENV and device overrides
* [pb env vault create](pb_env_vault_create.md)	 - Create a named ENV vault
* [pb env vault delete](pb_env_vault_delete.md)	 - Delete a named ENV vault and its values
* [pb env vault list](pb_env_vault_list.md)	 - List the workspace's named ENV vaults

