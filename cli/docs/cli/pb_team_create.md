## pb team create

Create a team by paying for its seats

### Synopsis

Create a team by paying for its seats. The command prints a Checkout link, opens it in a
browser when it can, and then waits until payment completes and the team exists. The owner uses
one of the seats.

Use --no-wait to print the link and return. Ctrl+C or --wait-timeout stops only the waiting; the
team is still created once you pay, and pb team get shows it. Running the same command again
reuses the same order instead of billing twice.

JSON output is supported with --json.

```
pb team create <team> [flags]
```

### Examples

```
  pb team create acme --seats 5
  pb team create acme --seats 5 --interval year --no-wait
  pb team create acme --json
```

### Options

```
  -h, --help                    help for create
      --interval string         billing interval: month or year (default "month")
      --json                    print canonical JSON
      --no-wait                 print the Checkout link and return without waiting for payment
      --seats int               seats to buy, including the owner's (1-128) (default 1)
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

