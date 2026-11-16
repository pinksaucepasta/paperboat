## pb config repository connect

Check and connect an HTTPS, SSH, or local Git repository

### Synopsis

Check and connect an HTTPS, SSH, or local Git repository

Check, then register an HTTPS, HTTP, SSH URL or absolute local Git path without embedded credentials. The repository is read first: it must have the branch with a commit, and a .paperboat/config-sync.toml on it, if present, must be valid. HTTP(S) and local repositories are read anonymously unless --credentials-file gives a profile (required for SSH); that profile is then saved on this machine. Use --name and --branch to select its display name and branch.

Configuration commands distinguish local CLI settings from synchronized repository content. Inspect current state before changing it; synchronization reports conflicts instead of silently overwriting one side. The --json option provides structured output for scripts where supported.

JSON output is supported with --json.

```
pb config repository connect <url-or-absolute-path> [flags]
```

### Options

```
      --branch string             Git branch (default "main")
      --credentials-file string   private credential profile used to check the repository and saved on this machine; - for stdin (required for SSH)
  -h, --help                      help for connect
      --json                      print JSON
      --name string               display name
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb config repository](pb_config_repository.md)	 - Connect custom Git repositories and manage machine credentials

