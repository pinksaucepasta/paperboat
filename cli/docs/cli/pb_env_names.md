## pb env names

List the names of the ENV variables commands on this machine receive (never values)

### Synopsis

Print the names of the ENV variables that `pb env run` and the shell hook put into commands on this machine for the active workspace: your team's, your own and this machine's, merged the way they are delivered. Values, their length and their hash are never printed, so a person or an AI agent can see which secret to use (for example STRIPE_API_KEY) without seeing it. --json adds the layer each name comes from: team, personal or machine.

A workspace owner can turn this off ("Commands on machines can list ENV names" on the Environment page); then this command says so and prints nothing.

How an AI agent finds and uses a secret:
  pb env names                       which ENV names commands here receive (never values)
  pb env run -- <command>            run a command with those values in its environment
  printf %s "$SECRET" | pb env set NAME --value-stdin     store a secret the user gives you
Never print, echo or write a value to a file or log: let the program that needs it read it from its environment.
`pb env run` gives a command the whole workspace set; there is no per-name scoping.

JSON output is supported with --json.

```
pb env names [flags]
```

### Examples

```
  pb env names
  pb env names --json
  pb env names | grep -q '^STRIPE_API_KEY$' && pb env run -- ./charge.sh
```

### Options

```
  -h, --help   help for names
      --json   print the names and the layer each comes from as JSON
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb env](pb_env.md)	 - Manage automatic global ENV and device overrides

