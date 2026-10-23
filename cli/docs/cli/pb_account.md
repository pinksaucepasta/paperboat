## pb account

Manage your Paperboat account

### Synopsis

Commands for your Paperboat account itself, as opposed to its machines, teams or sessions. Deleting the account removes everything it owns and is done only in the Paperboat console, where you confirm it with your email; `pb account delete` prints the link.

With --json, this command group lists its available commands.

```
pb account [flags]
```

### Options

```
  -h, --help   help for account
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

* [pb](pb.md)	 - Open Paperboat or connect to an environment terminal
* [pb account delete](pb_account_delete.md)	 - Show where to delete your Paperboat account

