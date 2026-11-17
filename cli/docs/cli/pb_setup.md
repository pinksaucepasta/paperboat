## pb setup

Set up this machine for Paperboat

### Synopsis

Enroll this machine in the signed-in account and set it up: create its Inbox, install the
running executable as the Paperboat service, and register its managed SSH target. Sign in with
pb login first. Flags choose the machine name and which incoming services it accepts, and
--ssh-port names the loopback port of the SSH server already running here (default 22). A recovery key is written only where
--recovery-output says; store it carefully.

On Windows, setup prepares OpenSSH first and may ask for administrator approval. On macOS a
hostname such as Name.local is shortened to its first label for the machine name.

JSON output is supported with --json.

```
pb setup [flags]
```

### Examples

```
  pb setup --name studio
  pb setup --name studio --ssh-port 2222 --managed-ssh=false
  pb setup --recovery-output /root/pb-recovery.txt --json
```

### Options

```
      --file-receive             accept native Inbox transfers (default true)
  -h, --help                     help for setup
      --json                     print JSON
      --managed-ssh              accept managed SSH tools (default true)
      --name string              machine name
      --preview-tunnel           serve previews and tunnels (default true)
      --recovery-output string   new absolute file for the account recovery key
      --ssh-port uint            existing loopback sshd port (default 22)
      --state-root string        runtime state directory
      --terminal                 accept Paperboat terminal and exec (default true)
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

