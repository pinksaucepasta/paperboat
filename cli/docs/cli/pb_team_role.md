## pb team role

Change a team member's role

### Synopsis

Change one member's role: admin, billing, auditor or member. Admins may only change
billing admins, auditors and members; appointing admins is an owner's job. The role changes who
manages the team, not the resource grants the member already has.
Pass the team's current generation with --generation (read it with pb team get). A stale
generation is rejected, so two people editing the team never overwrite each other.

JSON output is supported with --json.

```
pb team role <team> <account> <admin|billing|auditor|member> [flags]
```

### Examples

```
  pb team role acme ana auditor --generation 8
```

### Options

```
      --generation uint   expected current team generation
  -h, --help              help for role
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

