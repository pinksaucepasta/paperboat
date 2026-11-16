## pb recipe list

List the workspace's recipes

### Synopsis

List the recipes of the active workspace (or --workspace) with their tags, how many
machines each has paired (of its max_machines), when its token expires and when it
was last used. Tokens are never shown again after create and rotate; the list prints
only their public id.

JSON output is supported with --json.

```
pb recipe list [flags]
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

* [pb recipe](pb_recipe.md)	 - Create and manage recipes that pair and set up machines

