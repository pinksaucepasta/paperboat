## pb inbox policy

Show or update team file acceptance

### Synopsis

Show or set how team file requests are accepted. With no argument it prints the current
setting. manual means you approve each request yourself; automatic accepts them. The setting is
account-wide, not per machine.

Setting the policy also sets the receipt email to match --receipt-email: pass it to send
receipt email, leave it out to turn receipt email off.

JSON output is supported with --json.

```
pb inbox policy [manual|automatic] [flags]
```

### Examples

```
  pb inbox policy
  pb inbox policy automatic
  pb inbox policy manual --receipt-email
```

### Options

```
  -h, --help            help for policy
      --json            print JSON
      --receipt-email   send receipt email (applies when setting acceptance; omitted turns it off)
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb inbox](pb_inbox.md)	 - Manage the Paperboat Inbox

