## pb team invite

Invite a member by email, including before signup

### Synopsis

Invite a person by email address. They do not need an account yet: they sign in, verify
that email address, then accept the invitation. The invitation is for the member role and
expires; change the role afterwards with pb team role.

The team needs a spare seat, otherwise the invitation is refused and you must add seats first.
Pass the team's current generation with --generation (read it with pb team get). A stale
generation is rejected, so two people editing the team never overwrite each other.

JSON output is supported with --json.

```
pb team invite <team> <email> [flags]
```

### Examples

```
  pb team get acme
  pb team invite acme ana@example.test --generation 7
```

### Options

```
      --generation uint   expected current team generation
  -h, --help              help for invite
      --json              print canonical JSON
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

