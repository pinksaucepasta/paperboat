## pb recipe create

Create a recipe from a file and print its token once

### Synopsis

Create a recipe from a .jsonc file (JSON with comments); its name is the file's base name (ci.jsonc
makes ci). The file is checked on this computer first, with file:line:col errors.
The token is made here and printed once: save it as the secret PB_RECIPE. Paperboat
stores only a hash of it and can never read the ENV vaults the recipe reads.

JSON output is supported with --json.

```
pb recipe create -f <file> [flags]
```

### Examples

```
  pb recipe create -f ci.jsonc
  pb recipe create -f ci.jsonc --workspace acme
```

### Options

```
  -f, --file string   the recipe file (.jsonc)
  -h, --help          help for create
      --json          print JSON (the token included, once)
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

