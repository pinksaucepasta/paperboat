## pb recipe update

Change a recipe from its file

### Synopsis

Change a recipe to what its file now says; the recipe is named by the file's base
name. The token stays the same. Online machines from the recipe apply the new
revision at once (tags change server-side; ENV, config, preview and tunnel sections
are created, changed or removed on each machine).

JSON output is supported with --json.

```
pb recipe update -f <file> [flags]
```

### Examples

```
  pb recipe update -f ci.toml
```

### Options

```
  -f, --file string   the recipe file (.toml or .json)
  -h, --help          help for update
      --json          print JSON
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

