## pb team create

Create a team by paying for its seats

### Synopsis

Create a team. Teams are paid for first: this prints a secure Checkout link for the seats, and the team exists as soon as payment completes. Running the same command again reuses the same order.

Create a named team with this account as its initial owner. Membership and resource grants are managed separately after creation.

Teams use explicit membership and resource grants. Membership alone does not grant machine use, ENV values, previews, or tunnels. Mutations use the current team generation to reject stale edits; read pb team get before retrying a conflicting change.

JSON output is supported with --json.

```
pb team create <team> [flags]
```

### Options

```
  -h, --help                    help for create
      --interval string         billing interval: month or year (default "month")
      --json                    print canonical JSON
      --no-wait                 print the Checkout link and return without waiting for payment
      --seats int               seats to buy, including the owner's (default 1)
      --wait-timeout duration   how long to wait for payment before returning (default 15m0s)
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb team](pb_team.md)	 - Manage teams and explicit resource permissions

