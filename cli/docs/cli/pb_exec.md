## pb exec

Execute an exact command on a machine

### Synopsis

Run a command on a machine without a terminal session: pb exec <machine> -- <command...>.
The -- is required, and the arguments after it run exactly as given, with no shell unless you
name one. Output and the exit status come from the remote command.

--cwd sets the working directory and must be absolute in the machine's own form: /... on
Unix, a drive letter on Windows. --env NAME=value sets a variable (repeatable; valid names,
each once). --timeout bounds the whole run, including its start (at most 24h). --pty allocates
a remote terminal sized like this one (80x24 when unknown). --json prints
paperboat.exec-event/v1 events, one JSON object per line.

Ctrl-C cancels the remote command; a second Ctrl-C stops waiting and exits at once. A short
network break is reattached automatically.

Exit status: the remote command's own; 124 when --timeout ran out; 130 when cancelled; 125 when
the machine refused or could not start the command; 255 when the connection failed and the
command did not run.

JSON output is supported with --json.

```
pb exec <machine> [flags] -- <argv...>
```

### Examples

```
  pb exec homelab -- uname -a
  pb exec homelab --cwd /srv/app -- systemctl restart app
  pb exec homelab --timeout 30s -- ./migrate.sh
  pb exec winbox --cwd 'C:\Deploy' -- setup.exe /quiet
```

### Options

```
      --cwd string         working directory on the machine, absolute in its own form (/... or a drive letter); default: its workspace root
      --env stringArray    set a remote environment variable NAME=value (repeatable; each name once)
  -h, --help               help for exec
      --json               emit paperboat.exec-event/v1 JSON Lines
      --pty                allocate a remote terminal sized like this one
      --timeout duration   stop the command after this long, including its start (maximum 24h)
```

### Options inherited from parent commands

```
      --config string      path to the CLI config file
      --no-customization   ignore local shortcuts, command defaults, and TUI preferences
      --server string      paperboat-server base URL override
      --workspace string   resource workspace selector: personal or a team slug
```

### SEE ALSO

* [pb](pb.md)	 - Open Paperboat or connect to an environment terminal

