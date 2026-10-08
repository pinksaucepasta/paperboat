## pb machine list

List enrolled machines

### Synopsis

List enrolled machines

Show enrolled machines and their current account-visible state. Use the returned machine identity for rename, capabilities, availability, or revoke.

Machines are account enrollments with independent names, capabilities, and revocation state. Changes target the selected machine, while pb setup configures the current machine. Revocation disconnects an enrollment rather than just hiding it from the list.

JSON output is supported with --json.

```
pb machine list [flags]
```

### Options

```
  -h, --help           help for list
      --json           print JSON
      --owner string   filter owner: mine, shared, or an authorized account ID
      --q string       filter by resource name or ID
      --state string   filter by resource state
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb machine](pb_machine.md)	 - Manage machines

