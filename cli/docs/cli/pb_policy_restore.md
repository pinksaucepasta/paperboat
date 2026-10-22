## pb policy restore

Restore an earlier policy version

### Synopsis

Save an earlier policy version as the new current version. The history keeps
every version, so a restore can itself be undone. When the restore changes
anyone's access, the changes are printed and must be confirmed; --yes confirms
them without asking. pb policy versions lists the versions.

This command does not support --json output; it rejects that mode before execution. Use --help --json for structured command help.

```
pb policy restore <version> [flags]
```

### Examples

```
  pb policy restore 3
  pb policy restore 3 --yes --message "undo"
```

### Options

```
  -h, --help             help for restore
      --message string   describe the restore in the version history
      --yes              confirm the printed access changes without asking
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

* [pb policy](pb_policy.md)	 - Read, test and change the workspace access policy

