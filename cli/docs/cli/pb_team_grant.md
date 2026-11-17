## pb team grant

Set or revoke an explicit resource permission

### Synopsis

Give one member a permission on one resource, or revoke it with --active=false. The
permission is read or write for an ENV vault (env), and use, manage, inspect or replay for a
preview or tunnel. Each permission stands alone: manage does not imply inspect, and inspect does
not imply replay. Team membership alone grants none of them.
Pass the team's current generation with --generation (read it with pb team get). A stale
generation is rejected, so two people editing the team never overwrite each other.

JSON output is supported with --json.

```
pb team grant <team> <account> <env|preview|tunnel> <resource> <permission> [flags]
```

### Examples

```
  pb team grant acme ana env vault1 read --generation 9
  pb team grant acme ana preview blog manage --generation 9
  pb team grant acme ana preview blog manage --active=false --generation 10
```

### Options

```
      --active            grant permission; set false to revoke (default true)
      --generation uint   expected current team generation
  -h, --help              help for grant
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

