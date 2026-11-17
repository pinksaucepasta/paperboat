## pb team machine unshare

Withdraw the team's grants while retaining personal ownership

### Synopsis

Withdraw the team's access to a machine of yours. Its grants end and you stay the owner.
Pass the team's current generation with --generation (read it with pb team get). A stale
generation is rejected, so two people editing the team never overwrite each other.

JSON output is supported with --json.

```
pb team machine unshare <team> <machine-id> [flags]
```

### Examples

```
  pb team machine unshare acme m_abc123 --generation 9
```

### Options

```
      --generation uint   expected current team generation
  -h, --help              help for unshare
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

* [pb team machine](pb_team_machine.md)	 - Share machines and manage exact team access

