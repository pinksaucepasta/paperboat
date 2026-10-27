#!/bin/bash
# Plain ssh from a container to its own sshd as host alias "selfssh" (key login, no prompts): the controller side of env_ssh_hook.py and env_vscode_remote.py.
set -euo pipefail
mkdir -p ~/.ssh && chmod 700 ~/.ssh
[ -f ~/.ssh/id_self ] || ssh-keygen -q -t ed25519 -N '' -f ~/.ssh/id_self
grep -qF "$(cat ~/.ssh/id_self.pub)" ~/.ssh/authorized_keys 2>/dev/null || cat ~/.ssh/id_self.pub >> ~/.ssh/authorized_keys
printf 'Host selfssh\n  HostName 127.0.0.1\n  IdentityFile ~/.ssh/id_self\n  IdentitiesOnly yes\n  StrictHostKeyChecking accept-new\n' > ~/.ssh/config
chmod 600 ~/.ssh/authorized_keys ~/.ssh/config
ssh -o BatchMode=yes selfssh true
